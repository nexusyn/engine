package api

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AdminSuspendHandler — PATCH /v1/admin/orgs/{id}/suspend (suspend=true) e /unsuspend
// (suspend=false). Marca/desmarca a org como suspensa (billing). Control-plane
// (ADMIN-ONLY via RequireAbility no router); chamado pelo console (HandleStripeWebhook
// → NexusClient.setOrgSuspended) quando a assinatura falha/é cancelada. Usa a função
// SECURITY DEFINER set_org_suspended (0030) pra escrever cross-org bypassando RLS.
// AUD-010: antes o console chamava este PATCH mas o engine não o tinha (no-op) → org
// suspensa seguia operando. O gate de fato vive em checkQueryQuota + IngestHandler.
func AdminSuspendHandler(pool *pgxpool.Pool, suspend bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
		if err != nil || orgID <= 0 {
			writeError(w, http.StatusBadRequest, "org id inválido")
			return
		}
		if _, err := pool.Exec(r.Context(), "SELECT set_org_suspended($1, $2)", orgID, suspend); err != nil {
			writeInternalError(w, "set suspended", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "org_id": orgID, "suspended": suspend})
	}
}
