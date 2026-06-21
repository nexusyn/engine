package api

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AdminDeleteOrgHandler — DELETE /v1/admin/orgs/{id}: purge IRREVERSÍVEL de uma
// org e TODOS os seus dados (cascade). Control-plane (ADMIN-ONLY via
// RequireAbility no router); chamado pelo console na execução da exclusão de
// conta pós-carência (LGPD — direito à eliminação). Usa a função SECURITY
// DEFINER delete_org (0028). Idempotente: org inexistente → deleted:false (200),
// não 404, pra o job de exclusão poder reprocessar sem erro.
func AdminDeleteOrgHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
		if err != nil || orgID <= 0 {
			writeError(w, http.StatusBadRequest, "org id inválido")
			return
		}

		var deleted int64
		if err := pool.QueryRow(r.Context(), "SELECT delete_org($1)", orgID).Scan(&deleted); err != nil {
			writeInternalError(w, "delete org", err)
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"org_id":  orgID,
			"deleted": deleted > 0,
		})
	}
}
