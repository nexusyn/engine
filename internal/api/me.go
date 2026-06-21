package api

import (
	"encoding/json"
	"net/http"

	"github.com/nexusyn/engine/internal/tenant"
)

// MeResponse devolve dados do token autenticado — útil pra clientes
// verificarem qual org/token estão usando, e pra smoke-test do auth flow.
type MeResponse struct {
	OrganizationID int64 `json:"organization_id"`
	TokenID        int64 `json:"token_id"`
}

// MeHandler retorna a identidade do request autenticado.
// Falha com 401 se ctx não tem org_id (não deveria — Middleware bloquearia antes).
func MeHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, err := tenant.OrgIDFromContext(r.Context())
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(MeResponse{
			OrganizationID: orgID,
			TokenID:        tenant.TokenIDFromContext(r.Context()),
		})
	}
}
