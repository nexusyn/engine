package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nexusyn/engine/internal/tenant"
)

// graphNode é um nó pro front (cytoscape).
type graphNode struct {
	ID     int64  `json:"id"`
	Slug   string `json:"slug"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Degree int    `json:"degree"`
}

// graphEdge é uma aresta pro front.
type graphEdge struct {
	From int64  `json:"from"`
	To   int64  `json:"to"`
	Kind string `json:"kind"`
}

// GraphHandler — GET /v1/graph?limit=N: subgrafo INICIAL pra renderizar — os top-N
// god-nodes (entidades mais conectadas) + as arestas ENTRE eles. É o ponto de
// partida da navegação visual; expandir cada nó é GET /v1/entities/{slug}/related.
// 14k nós não cabem de uma vez, então começamos pelos centrais. RLS via contexto.
func GraphHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, err := tenant.OrgIDFromContext(r.Context())
		if err != nil {
			writeError(w, http.StatusUnauthorized, "no tenant in context")
			return
		}
		pg := parsePageParams(r, 40, 150) // default 40 nós, máx 150

		nodes := []graphNode{}
		edges := []graphEdge{}
		err = tenant.RunWithTenantReadOnly(r.Context(), pool, orgID, func(tx pgx.Tx) error {
			rows, e := tx.Query(r.Context(), `
				SELECT id, slug, name, kind, degree FROM entities
				WHERE valid_to IS NULL AND degree > 0
				ORDER BY degree DESC LIMIT $1`, pg.Limit)
			if e != nil {
				return e
			}
			ids := make([]int64, 0, pg.Limit)
			for rows.Next() {
				var n graphNode
				if se := rows.Scan(&n.ID, &n.Slug, &n.Name, &n.Kind, &n.Degree); se != nil {
					rows.Close()
					return se
				}
				nodes = append(nodes, n)
				ids = append(ids, n.ID)
			}
			rows.Close()
			if e := rows.Err(); e != nil {
				return e
			}
			if len(ids) == 0 {
				return nil
			}
			// arestas só ENTRE os nós já carregados (mantém o subgrafo coeso)
			erows, e := tx.Query(r.Context(), `
				SELECT DISTINCT from_entity_id, to_entity_id, kind FROM edges
				WHERE valid_to IS NULL
				  AND from_entity_id = ANY($1) AND to_entity_id = ANY($1)
				  AND from_entity_id <> to_entity_id`, ids)
			if e != nil {
				return e
			}
			defer erows.Close()
			for erows.Next() {
				var ed graphEdge
				if se := erows.Scan(&ed.From, &ed.To, &ed.Kind); se != nil {
					return se
				}
				edges = append(edges, ed)
			}
			return erows.Err()
		})
		if err != nil {
			writeInternalError(w, "graph", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"nodes": nodes, "edges": edges,
			"node_count": len(nodes), "edge_count": len(edges),
		})
	}
}

// nodeMemory é uma memória (page) ligada a uma entidade.
type nodeMemory struct {
	ID     int64  `json:"id"`
	Title  string `json:"title"`
	Domain string `json:"domain"`
}

// EntityMemoriesHandler — GET /v1/entities/{slug}/memories: as memórias (pages) de
// onde uma entidade foi extraída. É o que liga o nó do grafo de volta ao conteúdo —
// clicar num nó → ver/abrir as memórias que o mencionam. RLS via contexto.
func EntityMemoriesHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, err := tenant.OrgIDFromContext(r.Context())
		if err != nil {
			writeError(w, http.StatusUnauthorized, "no tenant in context")
			return
		}
		slug := chi.URLParam(r, "slug")
		if slug == "" {
			writeError(w, http.StatusBadRequest, "slug obrigatório")
			return
		}
		pg := parsePageParams(r, 50, 200)

		out := []nodeMemory{}
		err = tenant.RunWithTenantReadOnly(r.Context(), pool, orgID, func(tx pgx.Tx) error {
			var eid int64
			if e := tx.QueryRow(r.Context(),
				`SELECT id FROM entities WHERE slug = $1`, slug).Scan(&eid); e != nil {
				return e
			}
			rows, e := tx.Query(r.Context(), `
				SELECT DISTINCT p.id, p.title, p.domain
				FROM edges e JOIN pages p ON p.id = e.source_page_id
				WHERE (e.from_entity_id = $1 OR e.to_entity_id = $1)
				  AND e.valid_to IS NULL AND e.source_page_id IS NOT NULL
				ORDER BY p.id DESC LIMIT $2`, eid, pg.Limit)
			if e != nil {
				return e
			}
			defer rows.Close()
			for rows.Next() {
				var m nodeMemory
				if se := rows.Scan(&m.ID, &m.Title, &m.Domain); se != nil {
					return se
				}
				out = append(out, m)
			}
			return rows.Err()
		})
		if err != nil {
			if err == pgx.ErrNoRows {
				writeError(w, http.StatusNotFound, "entity não encontrada")
				return
			}
			writeInternalError(w, "entity memories", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"count": len(out), "memories": out})
	}
}
