package api

import (
	"encoding/json"
	"net/http"

	"github.com/nexusyn/engine/internal/core/query"
	"github.com/nexusyn/engine/internal/eval"
	"github.com/nexusyn/engine/internal/provider/llm"
	"github.com/nexusyn/engine/internal/tenant"
)

// EvalHandler — POST /v1/eval: roda o mini-bench embutido (LongMemEval, 12
// casos) com o modelo de geração RESOLVIDO da org (org_model_config > default
// global) + um juiz independente. Retorna accuracy + latência por categoria.
// É o "test performance" do dashboard — permite à pessoa comparar o desempenho
// do modelo/key que ela configurou.
func EvalHandler(querySvc *query.Service, judge llm.Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, err := tenant.OrgIDFromContext(r.Context())
		if err != nil {
			writeError(w, http.StatusUnauthorized, "no tenant in context")
			return
		}
		gen := querySvc.ResolveLLM(r.Context(), orgID)
		if gen == nil {
			writeError(w, http.StatusServiceUnavailable, "LLM provider não configurado")
			return
		}
		rep, err := eval.Run(r.Context(), gen, judge, query.SystemPrompt())
		if err != nil {
			writeInternalError(w, "eval", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rep)
	}
}
