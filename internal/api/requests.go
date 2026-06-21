package api

import (
	"net/http"

	"github.com/nexusyn/engine/internal/reqlog"
	"github.com/nexusyn/engine/internal/tenant"
)

// RequestsHandler — GET /v1/requests?limit=N: atividade recente da API (org),
// do ring buffer em memória. Backing da tela Requests.
func RequestsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, err := tenant.OrgIDFromContext(r.Context())
		if err != nil {
			writeError(w, http.StatusUnauthorized, "no tenant in context")
			return
		}
		pg := parsePageParams(r, 100, 1000)
		total := reqlog.Count(orgID)
		entries := reqlog.Recent(orgID, pg.Limit, pg.Offset)
		writeJSON(w, http.StatusOK, withPageMeta(
			map[string]any{"count": len(entries), "requests": entries},
			total, pg.Limit, pg.Offset, len(entries)))
	}
}
