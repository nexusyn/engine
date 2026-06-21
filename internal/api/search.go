package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nexusyn/engine/internal/core/search"
	"github.com/nexusyn/engine/internal/tenant"
)

// SearchRequest é o body do POST /v1/search.
type SearchRequest struct {
	Query  string `json:"query"`
	Limit  int    `json:"limit,omitempty"`
	Mode   string `json:"mode,omitempty"` // hybrid | vector | fts
	Domain string `json:"domain,omitempty"`
}

// SearchResponse é a resposta com lista de chunks.
type SearchResponse struct {
	Query   string          `json:"query"`
	Mode    string          `json:"mode"`
	Results []search.Result `json:"results"`
	Total   int             `json:"total"`
}

// SearchHandler executa busca híbrida e retorna chunks relevantes.
//
// Search CONTA na quota mensal de queries (decisão de produto 2026-06-12:
// embed+rerank também custam; quota única "X queries/mês" = qualquer retrieval).
func SearchHandler(svc *search.Service, pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, err := tenant.OrgIDFromContext(r.Context())
		if err != nil {
			writeError(w, http.StatusUnauthorized, "no tenant in context")
			return
		}

		var req SearchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
			return
		}
		defer func() { _ = r.Body.Close() }()

		if req.Query == "" {
			writeError(w, http.StatusBadRequest, "query é obrigatório")
			return
		}

		if !checkQueryQuota(w, r, pool, orgID) {
			return
		}

		start := time.Now()
		opts := search.Options{
			Query:  req.Query,
			Limit:  req.Limit,
			Mode:   search.Mode(req.Mode),
			Domain: req.Domain,
		}
		opts.Defaults()

		results, err := svc.Search(r.Context(), orgID, opts)
		if err != nil {
			writeInternalError(w, "search", err)
			return
		}

		// Metering: search não metrificava (relatório cego pra /v1/search).
		recordUsage(r.Context(), pool, orgID, 0, "search", "", "", int(time.Since(start).Milliseconds()), map[string]any{
			"results": len(results),
			"mode":    string(opts.Mode),
		})

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(SearchResponse{
			Query:   req.Query,
			Mode:    string(opts.Mode),
			Results: results,
			Total:   len(results),
		})
	}
}
