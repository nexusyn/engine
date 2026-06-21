package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
			return
		}
		defer func() { _ = r.Body.Close() }()

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
