// Package graph implementa traversal multi-hop em entities + edges.
//
// Day 16: fan-out via CTE recursiva limitada por MaxDepth, anti-ciclo via path.
// Day 17: bi-temporal via opção AsOf — filtra edges por valid_from/valid_to.
//
// Multi-hop é a feature que sistemas como Letta não têm. Permite queries como:
//
//	"Quais conceitos estão a 2 hops de distância da entity Luna?"
//	"Como era o grafo dela em 2024-01-01?"
package graph

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nexusyn/engine/internal/tenant"
)

// MaxAllowedDepth limita o fan-out pra evitar runaway queries.
// Graphs reais raramente precisam mais que 4-5 hops pra contexto útil.
const MaxAllowedDepth = 5

// Options parametriza a traversal.
type Options struct {
	// MaxDepth limita quantos hops o walk faz (default 2, max MaxAllowedDepth).
	MaxDepth int
	// Kinds filtra por kind de edge (vazio = todos).
	Kinds []string
	// AsOf filtra edges válidas naquele instante (default = now, ignora histórico).
	// Time-travel: ver o graph como estava num ponto no tempo.
	AsOf *time.Time
	// MaxResults limita o tamanho da response final (default 100).
	MaxResults int
}

func (o *Options) Defaults() {
	if o.MaxDepth <= 0 {
		o.MaxDepth = 2
	}
	if o.MaxDepth > MaxAllowedDepth {
		o.MaxDepth = MaxAllowedDepth
	}
	if o.MaxResults <= 0 {
		o.MaxResults = 100
	}
}

// Hop é uma entidade alcançada no walk, com metadados do caminho.
type Hop struct {
	EntityID int64    `json:"entity_id"`
	Slug     string   `json:"slug"`
	Name     string   `json:"name"`
	Kind     string   `json:"kind"`      // kind da entity de destino
	EdgeKind string   `json:"edge_kind"` // kind da edge que conectou
	Depth    int      `json:"depth"`
	Weight   float64  `json:"weight"`
	Path     []string `json:"path"` // slugs dos entities no caminho (incluso destino)
}

// Traverse executa fan-out a partir de fromEntityID via CTE recursiva.
// Resultado é distinct-by-entity-id pegando sempre o caminho MAIS CURTO.
func Traverse(ctx context.Context, pool *pgxpool.Pool, orgID, fromEntityID int64, opts Options) ([]Hop, error) {
	opts.Defaults()

	var hops []Hop
	err := tenant.RunWithTenantReadOnly(ctx, pool, orgID, func(tx pgx.Tx) error {
		results, err := traverseTx(ctx, tx, fromEntityID, opts)
		if err != nil {
			return err
		}
		hops = results
		return nil
	})
	return hops, err
}

func traverseTx(ctx context.Context, tx pgx.Tx, fromEntityID int64, opts Options) ([]Hop, error) {
	// asOf é NULL → planejar comparações com COALESCE pra que policy seja
	// "valid_to IS NULL OR valid_to > now()".
	var asOf any
	if opts.AsOf != nil {
		asOf = *opts.AsOf
	} else {
		asOf = nil
	}

	// Kinds vazio = sem filter. Postgres ANY(NULL) é false, então usamos
	// CARDINALITY check.
	var kindsArg any
	if len(opts.Kinds) > 0 {
		kindsArg = opts.Kinds
	} else {
		kindsArg = nil
	}

	query := `
WITH RECURSIVE walk AS (
    -- Hop inicial: edges saindo de fromEntityID
    SELECT
        e.to_entity_id              AS entity_id,
        e.kind                      AS edge_kind,
        e.weight                    AS weight,
        1                           AS depth,
        ARRAY[e.from_entity_id, e.to_entity_id] AS path_ids
    FROM edges e
    WHERE e.from_entity_id = $1
      AND (
          $3::timestamptz IS NULL
          AND e.valid_to IS NULL
          OR
          $3::timestamptz IS NOT NULL
          AND e.valid_from <= $3
          AND (e.valid_to IS NULL OR e.valid_to > $3)
      )
      AND ($4::text[] IS NULL OR cardinality($4::text[]) = 0 OR e.kind = ANY($4::text[]))

    UNION ALL

    -- Hops subsequentes: edges saindo de cada nó alcançado
    SELECT
        e2.to_entity_id,
        e2.kind,
        e2.weight,
        w.depth + 1,
        w.path_ids || e2.to_entity_id
    FROM edges e2
    JOIN walk w ON e2.from_entity_id = w.entity_id
    WHERE w.depth < $2
      AND NOT e2.to_entity_id = ANY(w.path_ids)
      AND (
          $3::timestamptz IS NULL
          AND e2.valid_to IS NULL
          OR
          $3::timestamptz IS NOT NULL
          AND e2.valid_from <= $3
          AND (e2.valid_to IS NULL OR e2.valid_to > $3)
      )
      AND ($4::text[] IS NULL OR cardinality($4::text[]) = 0 OR e2.kind = ANY($4::text[]))
),
shortest AS (
    -- Pra cada entity destino, pega o caminho de MENOR profundidade
    SELECT DISTINCT ON (entity_id)
        entity_id, edge_kind, weight, depth, path_ids
    FROM walk
    ORDER BY entity_id, depth ASC, weight DESC
)
SELECT
    s.entity_id, e.slug, e.name, e.kind,
    s.edge_kind, s.depth, s.weight, s.path_ids
FROM shortest s
JOIN entities e ON e.id = s.entity_id
ORDER BY s.depth ASC, e.name
LIMIT $5
`

	rows, err := tx.Query(ctx, query, fromEntityID, opts.MaxDepth, asOf, kindsArg, opts.MaxResults)
	if err != nil {
		return nil, fmt.Errorf("graph traverse: %w", err)
	}
	defer rows.Close()

	type rawHop struct {
		EntityID int64
		Slug     string
		Name     string
		Kind     string
		EdgeKind string
		Depth    int
		Weight   float64
		PathIDs  []int64
	}
	var raws []rawHop
	pathIDSet := map[int64]bool{fromEntityID: true}
	for rows.Next() {
		var r rawHop
		if err := rows.Scan(&r.EntityID, &r.Slug, &r.Name, &r.Kind, &r.EdgeKind, &r.Depth, &r.Weight, &r.PathIDs); err != nil {
			return nil, fmt.Errorf("graph traverse scan: %w", err)
		}
		raws = append(raws, r)
		for _, id := range r.PathIDs {
			pathIDSet[id] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Hidrata path IDs → slugs em 1 query única
	pathIDs := make([]int64, 0, len(pathIDSet))
	for id := range pathIDSet {
		pathIDs = append(pathIDs, id)
	}
	idToSlug, err := loadSlugMap(ctx, tx, pathIDs)
	if err != nil {
		return nil, err
	}

	out := make([]Hop, 0, len(raws))
	for _, r := range raws {
		pathSlugs := make([]string, 0, len(r.PathIDs))
		for _, id := range r.PathIDs {
			if s, ok := idToSlug[id]; ok {
				pathSlugs = append(pathSlugs, s)
			}
		}
		out = append(out, Hop{
			EntityID: r.EntityID,
			Slug:     r.Slug,
			Name:     r.Name,
			Kind:     r.Kind,
			EdgeKind: r.EdgeKind,
			Depth:    r.Depth,
			Weight:   r.Weight,
			Path:     pathSlugs,
		})
	}
	return out, nil
}

// loadSlugMap busca id → slug pra um conjunto de entity IDs num único SELECT.
func loadSlugMap(ctx context.Context, tx pgx.Tx, ids []int64) (map[int64]string, error) {
	if len(ids) == 0 {
		return map[int64]string{}, nil
	}
	rows, err := tx.Query(ctx, `SELECT id, slug FROM entities WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("graph: load slug map: %w", err)
	}
	defer rows.Close()
	out := make(map[int64]string, len(ids))
	for rows.Next() {
		var id int64
		var slug string
		if err := rows.Scan(&id, &slug); err != nil {
			return nil, err
		}
		out[id] = slug
	}
	return out, rows.Err()
}

// EntityIDFromSlug é helper pro endpoint: lookup id por slug (com RLS bind).
func EntityIDFromSlug(ctx context.Context, pool *pgxpool.Pool, orgID int64, slug string) (int64, error) {
	var id int64
	err := tenant.RunWithTenantReadOnly(ctx, pool, orgID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM entities WHERE slug = $1`, slug).Scan(&id)
	})
	return id, err
}

// ResolveEntityByName acha a entity current mais parecida com `name` por trigram
// (operador % → usa o índice GIN entities_name_trgm_idx). Retorna o id e o nome
// canônico. pgx.ErrNoRows quando nada casa (acima do threshold do GUC, 0.3).
// Usado por tools que recebem um nome livre do agente (ex: get_related no MCP).
func ResolveEntityByName(ctx context.Context, pool *pgxpool.Pool, orgID int64, name string) (int64, string, error) {
	var id int64
	var canonical string
	err := tenant.RunWithTenantReadOnly(ctx, pool, orgID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT id, name FROM entities
			WHERE name % $1 AND valid_to IS NULL
			ORDER BY similarity(name, $1) DESC
			LIMIT 1`, name).Scan(&id, &canonical)
	})
	return id, canonical, err
}
