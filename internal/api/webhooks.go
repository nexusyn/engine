package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nexusyn/engine/internal/tenant"
)

type webhook struct {
	ID        int64     `json:"id"`
	URL       string    `json:"url"`
	Event     string    `json:"event"`
	Active    bool      `json:"active"`
	CreatedAt time.Time `json:"created_at"`
}

// WebhooksListHandler — GET /v1/webhooks: lista os webhooks da org.
func WebhooksListHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, err := tenant.OrgIDFromContext(r.Context())
		if err != nil {
			writeError(w, http.StatusUnauthorized, "no tenant in context")
			return
		}
		pg := parsePageParams(r, 100, 500)
		out := []webhook{}
		total := 0
		err = tenant.RunWithTenantReadOnly(r.Context(), pool, orgID, func(tx pgx.Tx) error {
			if e := tx.QueryRow(r.Context(),
				`SELECT count(*) FROM webhooks WHERE organization_id = $1`, orgID).Scan(&total); e != nil {
				return e
			}
			rows, qerr := tx.Query(r.Context(),
				`SELECT id, url, event, active, created_at FROM webhooks
				 WHERE organization_id = $1 ORDER BY created_at DESC LIMIT $2 OFFSET $3`, orgID, pg.Limit, pg.Offset)
			if qerr != nil {
				return qerr
			}
			defer rows.Close()
			for rows.Next() {
				var h webhook
				if serr := rows.Scan(&h.ID, &h.URL, &h.Event, &h.Active, &h.CreatedAt); serr != nil {
					return serr
				}
				out = append(out, h)
			}
			return rows.Err()
		})
		if err != nil {
			writeInternalError(w, "webhooks list", err)
			return
		}
		writeJSON(w, http.StatusOK, withPageMeta(
			map[string]any{"count": len(out), "webhooks": out},
			total, pg.Limit, pg.Offset, len(out)))
	}
}

type createWebhookReq struct {
	URL   string `json:"url"`
	Event string `json:"event"`
}

// WebhookCreateHandler — POST /v1/webhooks {url, event}.
func WebhookCreateHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, err := tenant.OrgIDFromContext(r.Context())
		if err != nil {
			writeError(w, http.StatusUnauthorized, "no tenant in context")
			return
		}
		var req createWebhookReq
		if !decodeJSONBody(w, r, &req) {
			return
		}
		if req.URL == "" {
			writeError(w, http.StatusBadRequest, "url é obrigatória")
			return
		}
		if req.Event == "" {
			req.Event = "*"
		}
		var h webhook
		err = tenant.RunWithTenant(r.Context(), pool, orgID, func(tx pgx.Tx) error {
			return tx.QueryRow(r.Context(),
				`INSERT INTO webhooks (organization_id, url, event) VALUES ($1, $2, $3)
				 RETURNING id, url, event, active, created_at`,
				orgID, req.URL, req.Event,
			).Scan(&h.ID, &h.URL, &h.Event, &h.Active, &h.CreatedAt)
		})
		if err != nil {
			writeInternalError(w, "webhook create", err)
			return
		}
		writeJSON(w, http.StatusCreated, h)
	}
}

// WebhookDeleteHandler — DELETE /v1/webhooks/{id}.
func WebhookDeleteHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, err := tenant.OrgIDFromContext(r.Context())
		if err != nil {
			writeError(w, http.StatusUnauthorized, "no tenant in context")
			return
		}
		id, perr := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
		if perr != nil || id <= 0 {
			writeError(w, http.StatusBadRequest, "invalid webhook id")
			return
		}
		var deleted int64
		err = tenant.RunWithTenant(r.Context(), pool, orgID, func(tx pgx.Tx) error {
			ct, derr := tx.Exec(r.Context(),
				`DELETE FROM webhooks WHERE id = $1 AND organization_id = $2`, id, orgID)
			if derr != nil {
				return derr
			}
			deleted = ct.RowsAffected()
			return nil
		})
		if err != nil {
			writeInternalError(w, "webhook delete", err)
			return
		}
		if deleted == 0 {
			writeError(w, http.StatusNotFound, "webhook não encontrado")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "id": id})
	}
}
