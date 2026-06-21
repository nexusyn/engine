package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/nexusyn/engine/internal/job"
	"github.com/nexusyn/engine/internal/metering"
	"github.com/nexusyn/engine/internal/tenant"
)

// IngestRequest é o body do POST /v1/ingest.
//
// Filosofia: validation mínima aqui (campos obrigatórios). Lógica de domain default,
// chunking, embed scheduling fica no worker — handler é "thin" como acordado.
type IngestRequest struct {
	Title   string `json:"title"`
	Content string `json:"content"`
	Domain  string `json:"domain,omitempty"`
	// Agent = slug da IA que está salvando (claude/gemini/cursor/minimax/…),
	// find-or-create na org. É como a memória fica amarrada à IA fonte.
	Agent string `json:"agent,omitempty"`
	// AgentID = alternativa por id numérico direto. Agent (slug) tem precedência.
	AgentID  int64          `json:"agent_id,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// IngestResponse retorna o job_id criado pra que cliente possa polar status (futuro).
type IngestResponse struct {
	JobID   int64  `json:"job_id"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// IngestHandler enfileira um IngestJob via River.
//
// Não cria a page direto — ingest é async. Worker (`nexus serve --worker`)
// processa o job, faz chunking, insere page, schedula embed.
func IngestHandler(pool *pgxpool.Pool) http.HandlerFunc {
	// River client pra inserir jobs (sem consumir — workers rodam em processo separado)
	insertClient, err := job.NewInsertOnlyClient(pool)
	if err != nil {
		// Falha ao criar client é fatal — handler dispara 500 em todos requests
		return func(w http.ResponseWriter, _ *http.Request) {
			writeInternalError(w, "river init failed", err)
		}
	}

	return func(w http.ResponseWriter, r *http.Request) {
		orgID, err := tenant.OrgIDFromContext(r.Context())
		if err != nil {
			writeError(w, http.StatusUnauthorized, "no tenant in context")
			return
		}

		// AUD-010: org suspensa (billing) → 403, independente do enforce de cota.
		if !requireOrgActive(w, r, pool, orgID) {
			return
		}

		var req IngestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
			return
		}
		defer func() { _ = r.Body.Close() }()

		// Validation mínima
		if req.Content == "" {
			writeError(w, http.StatusBadRequest, "content é obrigatório")
			return
		}
		if req.Title == "" {
			req.Title = "Untitled" // default amigável
		}

		// Enforcement de storage (gated por NEXUS_ENFORCE_LIMITS; OFF por padrão).
		// max_pages = 0 (ou sem linha em org_limits) = ilimitado. Fail-open: erro
		// na checagem NUNCA bloqueia o ingest (não derruba cliente por bug nosso).
		if metering.Enforced() { // lido por request → reflete o toggle do admin em runtime
			var maxPages int64
			_ = pool.QueryRow(r.Context(),
				`SELECT coalesce(max_pages, 0) FROM org_limits WHERE organization_id = $1`, orgID).Scan(&maxPages)
			if maxPages > 0 {
				var used int64
				if pool.QueryRow(r.Context(),
					`SELECT count(*) FROM pages WHERE organization_id = $1`, orgID).Scan(&used) == nil && used >= maxPages {
					writeError(w, http.StatusPaymentRequired,
						"storage limit reached for your plan — upgrade to add more memories")
					return
				}
			}
		}

		// Resolve a IA fonte: slug (find-or-create) tem precedência sobre id numérico.
		agentID := req.AgentID
		if req.Agent != "" {
			id, aerr := resolveAgentID(r.Context(), pool, orgID, req.Agent)
			if aerr != nil {
				writeInternalError(w, "agent resolve", aerr)
				return
			}
			agentID = id
		}

		args := job.IngestArgs{
			OrganizationID: orgID,
			AgentID:        agentID,
			Title:          req.Title,
			Content:        req.Content,
			Domain:         req.Domain,
			Metadata:       req.Metadata,
		}

		// Insert o job — River vai persistir na DB e workers vão pickar
		result, err := insertClient.Insert(r.Context(), args, &river.InsertOpts{})
		if err != nil {
			writeInternalError(w, "enqueue", err)
			return
		}

		// Metering: 1 evento de ingest por memória salva (atribuído à IA).
		recordUsage(r.Context(), pool, orgID, agentID, "ingest", "", "", 0, nil)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(IngestResponse{
			JobID:   result.Job.ID,
			Status:  "queued",
			Message: "ingest enqueued, será processado em background",
		})
	}
}

// writeError é helper compartilhado pra responses de erro.
func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// writeInternalError loga o detalhe do erro (via slog) e devolve uma resposta
// GENÉRICA ao cliente — nunca o err.Error() cru, que vaza schema/SQLSTATE/infra
// do Postgres e lógica interna. `op` é um rótulo coarse da operação (vai só pro
// log, p/ correlação via request id). Toda falha 5xx do data plane passa por aqui.
func writeInternalError(w http.ResponseWriter, op string, err error) {
	slog.Error("api: internal error", "op", op, "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

// Sentinela pra evitar `errors` import nao usado num caminho que pode ser pruned
var _ = errors.New
