package graph

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nexusyn/engine/internal/tenant"
)

// Fase 4 (GraphRAG) — analytics estrutural do knowledge graph de uma org.
// Tudo sobre o grafo current (valid_to IS NULL), escopo por RLS. Barato: usa a
// coluna degree (0033) pros temas centrais; os conectores varrem as edges uma
// vez (não é hot path — é uma tool de panorama, não o retrieval).

// Overview é o panorama do grafo de uma org.
type Overview struct {
	Entities   int           `json:"entities"`    // entidades current
	Edges      int           `json:"edges"`       // arestas current
	ByKind     []KindCount   `json:"by_kind"`     // entidades por kind
	Central    []CentralNode `json:"central"`     // temas centrais (god-nodes, por grau)
	Connectors []Connector   `json:"connectors"`  // conectores (ligam tipos distintos)
}

type KindCount struct {
	Kind string `json:"kind"`
	N    int    `json:"n"`
}

// CentralNode é um "god-node": entidade mais conectada = tema central do tenant.
type CentralNode struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Degree int    `json:"degree"`
}

// Connector é uma entidade-ponte: liga muitos KINDS diferentes (proxy barato de
// betweenness — quem conecta domínios distintos da memória).
type Connector struct {
	Name          string `json:"name"`
	Kind          string `json:"kind"`
	Degree        int    `json:"degree"`
	KindDiversity int    `json:"kind_diversity"` // nº de kinds distintos na vizinhança
}

// GetOverview computa o panorama do grafo da org corrente (RLS via contexto).
// topN limita central e connectors (default 10, máx 50).
func GetOverview(ctx context.Context, pool *pgxpool.Pool, orgID int64, topN int) (*Overview, error) {
	if topN <= 0 {
		topN = 10
	}
	if topN > 50 {
		topN = 50
	}
	ov := &Overview{}
	err := tenant.RunWithTenantReadOnly(ctx, pool, orgID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM entities WHERE valid_to IS NULL`).Scan(&ov.Entities); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM edges WHERE valid_to IS NULL`).Scan(&ov.Edges); err != nil {
			return err
		}

		// entidades por kind
		rows, err := tx.Query(ctx, `
			SELECT kind, count(*) FROM entities WHERE valid_to IS NULL
			GROUP BY kind ORDER BY count(*) DESC`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var k KindCount
			if err := rows.Scan(&k.Kind, &k.N); err != nil {
				rows.Close()
				return err
			}
			ov.ByKind = append(ov.ByKind, k)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		// temas centrais (god-nodes) — coluna degree (0033), instantâneo
		rows, err = tx.Query(ctx, `
			SELECT name, kind, degree FROM entities
			WHERE valid_to IS NULL AND degree > 0
			ORDER BY degree DESC LIMIT $1`, topN)
		if err != nil {
			return err
		}
		for rows.Next() {
			var c CentralNode
			if err := rows.Scan(&c.Name, &c.Kind, &c.Degree); err != nil {
				rows.Close()
				return err
			}
			ov.Central = append(ov.Central, c)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		// conectores — diversidade de kinds da vizinhança (quem liga tipos distintos)
		rows, err = tx.Query(ctx, `
			WITH nbr AS (
				SELECT from_entity_id AS eid, to_entity_id AS other FROM edges WHERE valid_to IS NULL
				UNION ALL
				SELECT to_entity_id, from_entity_id FROM edges WHERE valid_to IS NULL
			)
			SELECT en.name, en.kind, en.degree, count(DISTINCT t.kind) AS kind_div
			FROM nbr
			JOIN entities en ON en.id = nbr.eid AND en.valid_to IS NULL
			JOIN entities t ON t.id = nbr.other AND t.valid_to IS NULL
			WHERE en.degree >= 3
			GROUP BY en.id, en.name, en.kind, en.degree
			ORDER BY kind_div DESC, en.degree DESC
			LIMIT $1`, topN)
		if err != nil {
			return err
		}
		for rows.Next() {
			var c Connector
			if err := rows.Scan(&c.Name, &c.Kind, &c.Degree, &c.KindDiversity); err != nil {
				rows.Close()
				return err
			}
			ov.Connectors = append(ov.Connectors, c)
		}
		rows.Close()
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return ov, nil
}
