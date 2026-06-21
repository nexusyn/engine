package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nexusyn/engine/internal/core/graph"
	"github.com/nexusyn/engine/internal/tenant"
)

// EntityRelatedResponse é o body do GET /v1/entities/:slug/related.
type EntityRelatedResponse struct {
	Entity EntityRef   `json:"entity"`
	Opts   QueryOpts   `json:"opts"`
	Hops   []graph.Hop `json:"hops"`
}

// EntityRef descreve a entity-âncora (de onde o walk começa).
type EntityRef struct {
	ID   int64  `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// QueryOpts ecoa parâmetros efetivos (depois de aplicar defaults).
type QueryOpts struct {
	MaxDepth int      `json:"max_depth"`
	Kinds    []string `json:"kinds,omitempty"`
	AsOf     string   `json:"as_of,omitempty"`
}

type entityListItem struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// EntitiesListHandler — GET /v1/entities?kind=X&limit=N&q=texto: lista as entidades
// da org (browse do grafo). Sem isso a tela Entities só funcionava buscando um slug
// conhecido. `q` filtra por substring (ILIKE) em nome OU slug — o campo de busca da
// tabela. Clicar numa entidade → /v1/entities/{slug}/related (traversal).
func EntitiesListHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, err := tenant.OrgIDFromContext(r.Context())
		if err != nil {
			writeError(w, http.StatusUnauthorized, "no tenant in context")
			return
		}
		kind := r.URL.Query().Get("kind")
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		pg := parsePageParams(r, 200, 500)

		// WHERE dinâmico compartilhado entre count e select (paginação correta).
		where := "organization_id = $1 AND valid_to IS NULL"
		args := []any{orgID}
		if kind != "" {
			args = append(args, kind)
			where += fmt.Sprintf(" AND kind = $%d", len(args))
		}
		if q != "" {
			args = append(args, "%"+q+"%")
			where += fmt.Sprintf(" AND (name ILIKE $%d OR slug ILIKE $%d)", len(args), len(args))
		}

		out := []entityListItem{}
		total := 0
		err = tenant.RunWithTenantReadOnly(r.Context(), pool, orgID, func(tx pgx.Tx) error {
			if e := tx.QueryRow(r.Context(),
				"SELECT count(*) FROM entities WHERE "+where, args...).Scan(&total); e != nil {
				return e
			}
			listArgs := append(append([]any{}, args...), pg.Limit, pg.Offset)
			rows, qerr := tx.Query(r.Context(), fmt.Sprintf(
				`SELECT slug, name, kind FROM entities
				 WHERE %s
				 ORDER BY kind, name LIMIT $%d OFFSET $%d`,
				where, len(args)+1, len(args)+2), listArgs...)
			if qerr != nil {
				return qerr
			}
			defer rows.Close()
			for rows.Next() {
				var e entityListItem
				if serr := rows.Scan(&e.Slug, &e.Name, &e.Kind); serr != nil {
					return serr
				}
				out = append(out, e)
			}
			return rows.Err()
		})
		if err != nil {
			writeInternalError(w, "entities list", err)
			return
		}
		writeJSON(w, http.StatusOK, withPageMeta(
			map[string]any{"count": len(out), "entities": out},
			total, pg.Limit, pg.Offset, len(out)))
	}
}

// EntityRelatedHandler implementa GET /v1/entities/:slug/related.
//
// Query params:
//   - depth   (int, default 2, max 5)
//   - kinds   (csv, ex: "mentions,located_in")
//   - as_of   (RFC3339 timestamp pra time-travel; omitido = now)
//   - limit   (int, default 100)
func EntityRelatedHandler(pool *pgxpool.Pool) http.HandlerFunc {
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

		opts := graph.Options{}
		if v := r.URL.Query().Get("depth"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				writeError(w, http.StatusBadRequest, "depth inválido")
				return
			}
			opts.MaxDepth = n
		}
		if v := r.URL.Query().Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				writeError(w, http.StatusBadRequest, "limit inválido")
				return
			}
			opts.MaxResults = n
		}
		if v := r.URL.Query().Get("kinds"); v != "" {
			for _, k := range strings.Split(v, ",") {
				k = strings.TrimSpace(strings.ToLower(k))
				if k != "" {
					opts.Kinds = append(opts.Kinds, k)
				}
			}
		}
		var asOfEcho string
		if v := r.URL.Query().Get("as_of"); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				writeError(w, http.StatusBadRequest, "as_of: use RFC3339 (ex: 2025-01-01T00:00:00Z)")
				return
			}
			opts.AsOf = &t
			asOfEcho = t.Format(time.RFC3339)
		}
		opts.Defaults() // pra ecoar os mesmos valores que Traverse vai usar

		// Lookup entity (ID + Name + Kind) por slug
		var ref EntityRef
		err = tenant.RunWithTenantReadOnly(r.Context(), pool, orgID, func(tx pgx.Tx) error {
			return tx.QueryRow(r.Context(),
				`SELECT id, slug, name, kind FROM entities WHERE slug = $1`, slug,
			).Scan(&ref.ID, &ref.Slug, &ref.Name, &ref.Kind)
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusNotFound, "entity não encontrada")
				return
			}
			writeInternalError(w, "lookup entity", err)
			return
		}

		hops, err := graph.Traverse(r.Context(), pool, orgID, ref.ID, opts)
		if err != nil {
			writeInternalError(w, "graph traverse", err)
			return
		}

		resp := EntityRelatedResponse{
			Entity: ref,
			Opts: QueryOpts{
				MaxDepth: opts.MaxDepth,
				Kinds:    opts.Kinds,
				AsOf:     asOfEcho,
			},
			Hops: hops,
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	}
}
