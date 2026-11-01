package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nexusyn/engine/internal/metering"
)

// AdminSetLimitsRequest — body do PUT /v1/admin/orgs/{id}/limits.
// 0 = ilimitado em qualquer campo.
type AdminSetLimitsRequest struct {
	MaxPages   int64 `json:"max_pages"`
	MaxQueries int64 `json:"max_queries"`
	MaxRps     int   `json:"max_rps"`
}

// AdminSetLimitsHandler — PUT /v1/admin/orgs/{id}/limits: grava a quota de plano
// de uma org. Control-plane (ADMIN-ONLY via RequireAbility no router); chamado
// pelo console (SyncTenantPlan) ao mudar de plano / provisionar. Usa a função
// SECURITY DEFINER set_org_limits (0020) pra escrever cross-org bypassando RLS.
func AdminSetLimitsHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
		if err != nil || orgID <= 0 {
			writeError(w, http.StatusBadRequest, "org id inválido")
			return
		}

		var req AdminSetLimitsRequest
		if !decodeJSONBody(w, r, &req) {
			return
		}

		if _, err := pool.Exec(r.Context(),
			"SELECT set_org_limits($1, $2, $3, $4)",
			orgID, req.MaxPages, req.MaxQueries, req.MaxRps,
		); err != nil {
			writeInternalError(w, "set limits", err)
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"ok":          true,
			"org_id":      orgID,
			"max_pages":   req.MaxPages,
			"max_queries": req.MaxQueries,
			"max_rps":     req.MaxRps,
		})
	}
}

// AdminSetPagesOverRequest — body do PUT /v1/admin/orgs/{id}/pages-over.
type AdminSetPagesOverRequest struct {
	Over bool `json:"over"`
}

// AdminSetPagesOverHandler marca estouro de memórias. Não arma o relógio:
// as 72h começam só no e-mail (arm_quota_notice). Limpa o relógio quando
// as memórias voltam pra dentro e as queries também. ADMIN-ONLY.
func AdminSetPagesOverHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
		if err != nil || orgID <= 0 {
			writeError(w, http.StatusBadRequest, "org id inválido")
			return
		}
		var req AdminSetPagesOverRequest
		if !decodeJSONBody(w, r, &req) {
			return
		}
		var overSince *time.Time
		if err := pool.QueryRow(r.Context(),
			"SELECT set_pages_over($1, $2)", orgID, req.Over,
		).Scan(&overSince); err != nil {
			writeInternalError(w, "set pages over", err)
			return
		}
		body := map[string]any{"ok": true, "org_id": orgID, "over": req.Over}
		if overSince != nil {
			body["over_since"] = overSince.UTC().Format(time.RFC3339)
		}
		writeJSON(w, http.StatusOK, body)
	}
}

// AdminArmQuotaNoticeRequest — body do PUT /v1/admin/orgs/{id}/quota-notice.
// noticed_at é o instante do e-mail (RFC3339). Vazio = now().
type AdminArmQuotaNoticeRequest struct {
	NoticedAt string `json:"noticed_at"`
}

// AdminArmQuotaNoticeHandler grava o relógio de 72h no instante do e-mail.
// Não reescreve um relógio já armado. ADMIN-ONLY.
func AdminArmQuotaNoticeHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
		if err != nil || orgID <= 0 {
			writeError(w, http.StatusBadRequest, "org id inválido")
			return
		}
		var req AdminArmQuotaNoticeRequest
		if !decodeJSONBody(w, r, &req) {
			return
		}
		var noticedAt *time.Time
		if req.NoticedAt != "" {
			t, err := time.Parse(time.RFC3339, req.NoticedAt)
			if err != nil {
				writeError(w, http.StatusBadRequest, "noticed_at inválido")
				return
			}
			noticedAt = &t
		}
		var overSince *time.Time
		if err := pool.QueryRow(r.Context(),
			"SELECT arm_quota_notice($1, $2)", orgID, noticedAt,
		).Scan(&overSince); err != nil {
			writeInternalError(w, "arm quota notice", err)
			return
		}
		if overSince == nil {
			writeError(w, http.StatusNotFound, "org sem limites")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":         true,
			"org_id":     orgID,
			"over_since": overSince.UTC().Format(time.RFC3339),
			"block_at":   overSince.Add(metering.QueryGrace).UTC().Format(time.RFC3339),
		})
	}
}
