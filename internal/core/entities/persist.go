package entities

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nexusyn/engine/internal/tenant"
)

// PersistStats reporta o que foi inserido vs. já existente.
type PersistStats struct {
	EntitiesInserted int
	EntitiesExisting int
	EdgesInserted    int
	EdgesSuperseded  int // Day 20: edges 1-to-1 antigas invalidadas (valid_to=now())
}

// Persist insere entities (UPSERT em conflito por slug, mergeando aliases)
// e edges (INSERT direto, bi-temporal — duplicatas são histórico válido).
//
// pageID é obrigatório (atrelado em edges.source_page_id).
// orgID precisa estar bindado no tx via tenant.RunWithTenant pelo caller.
func Persist(ctx context.Context, tx pgx.Tx, orgID, pageID int64, e *Extracted) (PersistStats, error) {
	stats := PersistStats{}

	// 1. UPSERT entities — slug é key de dedup
	nameToID := make(map[string]int64, len(e.Entities))
	for _, ent := range e.Entities {
		slug := ent.Slug()
		if slug == "" {
			continue
		}

		attrsJSON, err := json.Marshal(ent.Attributes)
		if err != nil {
			return stats, fmt.Errorf("entities persist: marshal attributes %s: %w", ent.Name, err)
		}
		if len(ent.Attributes) == 0 {
			attrsJSON = []byte(`{}`)
		}

		// Postgres TEXT[] NOT NULL: nil slice em Go vira NULL → viola constraint.
		// Garante slice vazio explícito.
		aliases := ent.Aliases
		if aliases == nil {
			aliases = []string{}
		}

		var id int64
		var wasInsert bool
		err = tx.QueryRow(ctx, `
			INSERT INTO entities (organization_id, slug, kind, name, aliases, attributes)
			VALUES ($1, $2, $3, $4, $5, $6::jsonb)
			ON CONFLICT (organization_id, slug) DO UPDATE
			SET aliases = (
				SELECT ARRAY(SELECT DISTINCT unnest(entities.aliases || EXCLUDED.aliases))
			),
			    attributes = entities.attributes || EXCLUDED.attributes
			RETURNING id, (xmax = 0) AS was_insert
		`,
			orgID, slug, ent.Kind, ent.Name, aliases, attrsJSON,
		).Scan(&id, &wasInsert)
		if err != nil {
			return stats, fmt.Errorf("entities persist: upsert %s: %w", ent.Name, err)
		}

		nameToID[ent.Name] = id
		if wasInsert {
			stats.EntitiesInserted++
		} else {
			stats.EntitiesExisting++
		}
	}

	// 2. INSERT edges (resolução Name → ID via map). Sem dedup: o modelo
	// bi-temporal aceita múltiplas edges com mesmo (from,to,kind) — cada
	// uma carrega seu valid_from e source_page_id distintos.
	//
	// Day 20 — supersede 1-to-1: pra kinds com cardinality 1-to-1
	// (located_in, works_for, ...), edges existentes com mesmo from+kind
	// mas to diferente ganham valid_to=now() ANTES da nova ser inserida.
	//
	// Day 21 — batch-aware: quando o LLM extrai N edges 1-to-1 contraditórias
	// na MESMA passada (ex: "Antes morava em Lavras, agora em Florianópolis"
	// gera 2 located_in), pre-filtra pra que só a ÚLTIMA do batch sobreviva.
	// Evita zombie edges (duration 0) no histórico.
	edgesToInsert := dedupOneToOneBatch(e.Edges)
	for _, edge := range edgesToInsert {
		fromID, okF := nameToID[edge.FromName]
		toID, okT := nameToID[edge.ToName]
		if !okF || !okT {
			continue // referência a entity não-persistida (sanitize falhou em pegar)
		}

		if IsOneToOne(edge.Kind) {
			// Invalida edges antigas (mesma from+kind, to diferente, ainda válidas)
			tag, err := tx.Exec(ctx, `
				UPDATE edges
				SET valid_to = now()
				WHERE organization_id = $1
				  AND from_entity_id = $2
				  AND kind = $3
				  AND to_entity_id <> $4
				  AND valid_to IS NULL
			`, orgID, fromID, edge.Kind, toID)
			if err != nil {
				return stats, fmt.Errorf("entities persist: supersede %s→%s: %w", edge.FromName, edge.ToName, err)
			}
			stats.EdgesSuperseded += int(tag.RowsAffected())
		}

		_, err := tx.Exec(ctx, `
			INSERT INTO edges (organization_id, from_entity_id, to_entity_id, kind, weight, source_page_id, confidence, confidence_score)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		`, orgID, fromID, toID, edge.Kind, edge.Weight, pageID, edge.Confidence, edge.ConfidenceScore)
		if err != nil {
			return stats, fmt.Errorf("entities persist: insert edge %s→%s: %w", edge.FromName, edge.ToName, err)
		}
		stats.EdgesInserted++
	}

	// Fase 1 (GraphRAG) — recalcula entities.degree das entities desta página, pro
	// hub-damping da graph-expansion ler de coluna (PK lookup) em vez de varrer as
	// edges a cada query. IDs ordenados pra reduzir deadlock entre workers paralelos.
	if len(nameToID) > 0 {
		ids := make([]int64, 0, len(nameToID))
		for _, id := range nameToID {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		if _, err := tx.Exec(ctx, `
			UPDATE entities e SET degree = (
				SELECT count(*) FROM edges d
				WHERE (d.from_entity_id = e.id OR d.to_entity_id = e.id) AND d.valid_to IS NULL
			) WHERE e.id = ANY($1)`, ids); err != nil {
			return stats, fmt.Errorf("entities persist: update degree: %w", err)
		}
	}

	return stats, nil
}

// MarkPageProcessed atualiza pages.entities_extracted_at = now().
// Chamado pelo job APÓS Persist sucesso (mesma transação).
func MarkPageProcessed(ctx context.Context, tx pgx.Tx, pageID int64) error {
	tag, err := tx.Exec(ctx, `UPDATE pages SET entities_extracted_at = now() WHERE id = $1`, pageID)
	if err != nil {
		return fmt.Errorf("entities mark-processed: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("entities mark-processed: page %d not found", pageID)
	}
	return nil
}

// dedupOneToOneBatch resolve contradições internas ao batch: pra cada
// (from_name, kind) onde kind é 1-to-1, mantém apenas a ÚLTIMA edge
// da lista de entrada. Edges M-to-M passam intactas (todas mantidas).
//
// Motivação: quando o LLM extrai "antes morava em Lavras, agora em
// Florianópolis" gera 2 edges located_in. Sem este filtro, a primeira
// é inserida e imediatamente superseded pela segunda → edge zombie
// com duration 0 no histórico. Com filtro: só florianopolis é inserida.
func dedupOneToOneBatch(edges []EdgeRef) []EdgeRef {
	// Acha índice da última ocorrência por (from_name + kind) — só pra 1-to-1
	lastIdx := make(map[string]int)
	for i, e := range edges {
		if !IsOneToOne(e.Kind) {
			continue
		}
		key := e.FromName + "\x00" + e.Kind
		lastIdx[key] = i
	}

	if len(lastIdx) == 0 {
		return edges // nenhum 1-to-1, nada a filtrar
	}

	out := make([]EdgeRef, 0, len(edges))
	for i, e := range edges {
		if !IsOneToOne(e.Kind) {
			out = append(out, e)
			continue
		}
		key := e.FromName + "\x00" + e.Kind
		if lastIdx[key] == i {
			out = append(out, e) // só inclui a última
		}
	}
	return out
}

// RunInTenantTx é açúcar pro caller: Persist + MarkPageProcessed numa tx
// com tenant bind. Mantém worker code enxuto.
func RunInTenantTx(ctx context.Context, pool *pgxpool.Pool, orgID, pageID int64, e *Extracted) (PersistStats, error) {
	var stats PersistStats
	err := tenant.RunWithTenant(ctx, pool, orgID, func(tx pgx.Tx) error {
		s, perr := Persist(ctx, tx, orgID, pageID, e)
		if perr != nil {
			return perr
		}
		stats = s
		return MarkPageProcessed(ctx, tx, pageID)
	})
	return stats, err
}
