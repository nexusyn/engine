package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nexusyn/engine/internal/tenant"
)

type eventItem struct {
	ID        int64          `json:"id"`
	Kind      string         `json:"kind"`
	Payload   map[string]any `json:"payload"`
	CreatedAt time.Time      `json:"created_at"`
}

// EventsListHandler — GET /v1/events?kind=<prefixo>&limit=<n>: audit log da org
// (tabela events). `kind` casa por PREFIXO (ex.: "guideline." pega added/updated/
// deleted). Tenant-scoped por RLS. Usado pelo console para surfacing de mudança
// de guideline (notificação ao dono) e para uma activity feed em geral.
func EventsListHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, err := tenant.OrgIDFromContext(r.Context())
		if err != nil {
			writeError(w, http.StatusUnauthorized, "no tenant in context")
			return
		}
		kind := strings.TrimSpace(r.URL.Query().Get("kind"))
		limit := 50
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, e := strconv.Atoi(v); e == nil && n > 0 {
				limit = n
			}
		}
		if limit > 200 {
			limit = 200
		}

		out := []eventItem{}
		err = tenant.RunWithTenantReadOnly(r.Context(), pool, orgID, func(tx pgx.Tx) error {
			sql := `SELECT id, kind, payload, created_at FROM events WHERE organization_id = $1`
			args := []any{orgID}
			if kind != "" {
				args = append(args, kind+"%")
				sql += " AND kind LIKE $2"
			}
			// limit é int (strconv.Itoa) — sem injeção; kind vai como $2 parametrizado.
			sql += " ORDER BY created_at DESC LIMIT " + strconv.Itoa(limit)
			rows, e := tx.Query(r.Context(), sql, args...)
			if e != nil {
				return e
			}
			defer rows.Close()
			for rows.Next() {
				var it eventItem
				var payload []byte
				if e := rows.Scan(&it.ID, &it.Kind, &payload, &it.CreatedAt); e != nil {
					return e
				}
				_ = json.Unmarshal(payload, &it.Payload)
				out = append(out, it)
			}
			return rows.Err()
		})
		if err != nil {
			writeInternalError(w, "events list", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"events": out})
	}
}
