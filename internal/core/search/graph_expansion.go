package search

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Fase 1 (GraphRAG, 2026-06-18) — graph-expansion como 5º canal RRF.
//
// O entity-match (3º canal) traz chunks das pages onde as entidades DA QUERY
// foram extraídas. Este canal vai além: pega essas entidades como SEEDS e
// expande 1–2 hops pelo grafo, trazendo chunks das pages das entidades
// VIZINHAS — memórias conectadas que vector/FTS não pegam porque não contêm os
// termos da query (ex.: "decisão X" ligada ao "erro Y" que a motivou).
//
// Disciplina anti-ruído (validada contra prod 2026-06-18 — sem ela a expansão
// divergia do tópico, puxando o cluster inteiro de uma sessão multi-assunto):
//   1. expande SÓ por relações de raciocínio/causa/dependência (graphExpandKinds)
//      — não por `uses`/`contains`/`part_of`/`mentions`, que co-ocorrem demais;
//   2. NÃO atravessa super-hubs — entidades com grau > graphHubDegree (≈p99) são
//      pontes que levam pra fora do assunto (Filament=144, console=230…);
//   3. gate de confidence (Fase 2) — arestas ambíguas não propagam.
//
// ⚠️ LIMITAÇÃO conhecida (2026-06-18): no grafo LEGADO o backfill marcou todas
// as arestas como extracted/1.0, então o gate (3) não filtra nada e a expansão
// ainda traz co-menções de sessões multi-assunto. O sinal só fica limpo após
// RE-EXTRAÇÃO com o worker novo (confidence real por aresta). Por isso a flag
// nasce OFF: ligar só após medir no bench / re-extrair o grafo.
//
// Flag NEXUS_GRAPH_EXPANSION (OFF por padrão) — modo observação: o canal só
// roda quando ligado, pra medir o ganho marginal (NEXUS_SEARCH_DEBUG) antes de
// virar o peso em prod.

// graphExpansionDefault é o default GLOBAL do canal (env, lido uma vez). Cada org
// pode sobrescrever via organizations.settings->>'graph_expansion' — ver
// graphExpansionEnabled. Permite rollout por-org (ligar só nas orgs com grafo
// re-extraído / confidence real), sem ligar pra todos de uma vez.
var graphExpansionDefault = os.Getenv("NEXUS_GRAPH_EXPANSION") == "1" || os.Getenv("NEXUS_GRAPH_EXPANSION") == "true"

// graphExpansionEnabled resolve a flag PARA A ORG CORRENTE (RLS): o override em
// organizations.settings->>'graph_expansion' (se presente) vence o default global.
// Falha graceful — erro/ausência → default global. +1 PK-lookup por search.
func graphExpansionEnabled(ctx context.Context, tx pgx.Tx) bool {
	var v *bool
	err := tx.QueryRow(ctx,
		`SELECT (settings->>'graph_expansion')::bool FROM organizations WHERE id = current_org_id()`,
	).Scan(&v)
	if err != nil || v == nil {
		return graphExpansionDefault
	}
	return *v
}

const (
	// graphSeedCap limita quantas entidades-seed da query iniciam a expansão.
	// Menor que o entityMatchCap (8): a expansão amplifica, então poucos seeds
	// de qualidade > muitos fracos.
	graphSeedCap = 5
	// graphSeedThreshold é a similaridade trigram mínima do seed (igual ao
	// entity-match — mesma lógica de NL longa vs name curto).
	graphSeedThreshold = 0.3
	// graphMaxDepth é o nº de hops a partir do seed. 2 = vizinhos + vizinhos-de-
	// vizinhos; além disso o sinal vira ruído.
	graphMaxDepth = 2
	// graphHopDecay multiplica o peso a cada hop (peso = sim · Πconfidence · decay^depth).
	graphHopDecay = 0.5
	// graphMinConfidence é o gate da Fase 2: arestas abaixo disso não propagam
	// (ambiguous=0.4 fica de fora; inferred=0.7 e extracted=1.0 passam).
	graphMinConfidence = 0.5
	// graphHubDegree é o teto de grau pra uma entidade ser atravessada na
	// expansão. Acima disso é super-hub (genérico, pouco discriminativo) e vira
	// ponte pra fora do tópico. ≈ p99 do grau na org 2 (36) com folga.
	graphHubDegree = 40
)

// graphExpandKinds são os edge kinds por onde a expansão caminha: relações de
// raciocínio/causa/dependência/preferência, que carregam significado real.
// Deliberadamente EXCLUI composição/uso (contains/part_of/uses) e genéricos
// (mentions/relates_to), que co-ocorrem demais e levam a expansão pra fora do
// tópico. Injetado como literal SQL (não user input).
var graphExpandKindsSQL = "'caused','depends_on','fixes','prevents','mitigates'," +
	"'enables','changed_from','contradicts','alternative_to','replaces'," +
	"'decided','prefers','avoids'"

// graphExpansionSearch retorna chunks alcançados expandindo o grafo a partir das
// entidades-seed da query. Falha graceful: erro de DB → nil+erro, tratado como
// sinal opcional pelo caller (runHybrid).
//
// asOf (time-travel): quando setado, seeds, arestas e pages são filtrados pelo
// snapshot temporal — igual ao entity-match.
func graphExpansionSearch(ctx context.Context, tx pgx.Tx, query string, limit int, domain string, asOf *time.Time) ([]int64, error) {
	if query == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 50
	}

	// Filtros temporais — current por padrão, snapshot quando asOf setado.
	entValidFilter := "AND e.valid_to IS NULL"
	edgeValidWalk := "AND edge.valid_to IS NULL"
	edgeValidPages := "AND edge.valid_to IS NULL"
	pageCreatedFilter := ""
	if asOf != nil {
		entValidFilter = "AND e.valid_from <= $2 AND (e.valid_to IS NULL OR e.valid_to > $2)"
		edgeValidWalk = "AND edge.valid_from <= $2 AND (edge.valid_to IS NULL OR edge.valid_to > $2)"
		edgeValidPages = "AND edge.valid_from <= $2 AND (edge.valid_to IS NULL OR edge.valid_to > $2)"
		pageCreatedFilter = "AND p.created_at <= $2"
	}

	args := []any{query}
	paramN := 2
	if asOf != nil {
		args = append(args, *asOf)
		paramN = 3
	}

	// seeds: entidades que casam com a query (trigram/alias), top graphSeedCap.
	// walk: CTE recursiva — depth 0 = seeds; hops seguem só arestas "ricas"
	//   (≠ mentions/relates_to) acima do gate de confidence, anti-ciclo via path.
	//   mult acumula sim · confidence_score · decay por hop.
	// reached: entidades a ≥1 hop, menor depth / maior mult.
	// expanded_pages: pages onde as vizinhas foram extraídas, peso = Σ mult.
	sql := fmt.Sprintf(`
		WITH RECURSIVE seeds AS (
			-- operador %% (não similarity()>thr) pra usar o índice GIN trgm
			-- entities_name_trgm_idx — senão é seq scan de todas as entities (~50ms).
			-- threshold = GUC pg_trgm.similarity_threshold (0.3 = graphSeedThreshold).
			-- Alias-match fica fora daqui de propósito: é o entity-match (canal
			-- primário) que cobre alias; aqui performance > recall marginal.
			SELECT e.id, similarity(e.name, $1) AS sim
			FROM entities e
			WHERE e.name %% $1
			  %s
			ORDER BY sim DESC
			LIMIT %d
		),
		walk AS (
			-- cast p/ double precision: o termo recursivo promove a double (decay
			-- literal), e a UNION recursiva exige tipo idêntico nos dois ramos.
			SELECT s.id AS entity_id, 0 AS depth, s.sim::double precision AS mult, ARRAY[s.id] AS path
			FROM seeds s
			UNION ALL
			-- hub-damping: o JOIN com entities lê o grau pré-computado (0033) por PK
			-- e poda super-hubs (degree > graphHubDegree) — sem varrer edges por query.
			SELECT edge.to_entity_id, w.depth + 1,
			       w.mult * edge.confidence_score * %g,
			       w.path || edge.to_entity_id
			FROM edges edge
			JOIN walk w ON edge.from_entity_id = w.entity_id
			JOIN entities te ON te.id = edge.to_entity_id
			WHERE w.depth < %d
			  AND NOT edge.to_entity_id = ANY(w.path)
			  AND edge.kind IN (%s)
			  AND edge.confidence_score >= %g
			  AND te.degree <= %d
			  %s
		),
		reached AS (
			SELECT DISTINCT ON (entity_id) entity_id, depth, mult
			FROM walk
			WHERE depth >= 1
			ORDER BY entity_id, depth ASC, mult DESC
		),
		expanded_pages AS (
			SELECT edge.source_page_id AS page_id, sum(r.mult) AS w
			FROM edges edge
			JOIN reached r ON r.entity_id = edge.from_entity_id OR r.entity_id = edge.to_entity_id
			WHERE edge.source_page_id IS NOT NULL
			  %s
			GROUP BY edge.source_page_id
		)
		SELECT c.id
		FROM chunks c
		JOIN expanded_pages ep ON ep.page_id = c.page_id
		JOIN pages p ON p.id = c.page_id
		WHERE TRUE
		  %s`,
		entValidFilter, graphSeedCap,
		graphHopDecay, graphMaxDepth, graphExpandKindsSQL, graphMinConfidence, graphHubDegree, edgeValidWalk,
		edgeValidPages, pageCreatedFilter)

	if domain != "" {
		args = append(args, domain)
		sql += fmt.Sprintf(" AND p.domain = $%d", paramN)
		paramN++
	}
	args = append(args, limit)
	sql += fmt.Sprintf(" ORDER BY ep.w DESC, c.position ASC LIMIT $%d", paramN)

	rows, err := tx.Query(ctx, strings.TrimSpace(sql), args...)
	if err != nil {
		return nil, fmt.Errorf("graph expansion search: %w", err)
	}
	defer rows.Close()

	out := make([]int64, 0, limit)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("graph expansion scan: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
