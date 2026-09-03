package api

import (
	"encoding/json"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nexusyn/engine/internal/core/query"
	"github.com/nexusyn/engine/internal/metering"
	"github.com/nexusyn/engine/internal/tenant"
)

// QueryRequest é o body do POST /v1/query.
type QueryRequest struct {
	Question string `json:"question"`
	Limit    int    `json:"limit,omitempty"`
	Mode     string `json:"mode,omitempty"` // hybrid | vector | fts
	Domain   string `json:"domain,omitempty"`
	// Project = filtra a busca por projeto (traz o projeto + as memórias globais). Opcional.
	Project string `json:"project,omitempty"`
	// Agent = slug da IA que está consultando (atribuição de uso/billing).
	Agent string `json:"agent,omitempty"`
	// MultiHop ativa decomposição em sub-queries via LLM (1 call extra).
	// Útil pra perguntas multi-fato. Default false.
	MultiHop bool `json:"multi_hop,omitempty"`
}

// QueryHandler executa search + rerank + LLM grounded. Recebe pool pra resolver
// o agent (atribuição) e gravar o metering em usage_records.
func QueryHandler(svc *query.Service, pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, err := tenant.OrgIDFromContext(r.Context())
		if err != nil {
			writeError(w, http.StatusUnauthorized, "no tenant in context")
			return
		}

		var req QueryRequest
		if !decodeJSONBody(w, r, &req) {
			return
		}

		if req.Question == "" {
			writeError(w, http.StatusBadRequest, "question é obrigatório")
			return
		}

		// Quota mensal de queries (402 se estourada e enforcement ligado).
		if !checkQueryQuota(w, r, pool, orgID) {
			return
		}

		// Atribui a query à IA (relatório/billing por agent). Best-effort.
		var agentID int64
		if req.Agent != "" {
			agentID, _ = resolveAgentID(r.Context(), pool, orgID, req.Agent)
		}

		resp, err := svc.Query(r.Context(), orgID, query.Options{
			Question: req.Question,
			Limit:    req.Limit,
			Mode:     req.Mode,
			Domain:   req.Domain,
			Project:  metering.SanitizeProjectSlug(req.Project),
			MultiHop: req.MultiHop,
		})
		if err != nil {
			writeInternalError(w, "query", err)
			return
		}

		// Metering: 1 evento de query, com provider/model/latência da geração.
		recordUsage(r.Context(), pool, orgID, agentID, "query", resp.Usage.LLMProvider, resp.Usage.LLMModel, resp.Usage.LatencyMs, map[string]any{
			"tokens_in":  resp.Usage.TokensIn,
			"tokens_out": resp.Usage.TokensOut,
			"reranked":   resp.Usage.Reranked,
		})

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	}
}
