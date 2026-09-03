package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nexusyn/engine/internal/core/query"
	"github.com/nexusyn/engine/internal/provider/llm"
	"github.com/nexusyn/engine/internal/tenant"
)

// QueryStreamHandler escreve eventos SSE conforme query.Service.QueryStream emite.
//
// Formato: `event: <kind>\ndata: {json}\n\n` por evento. Flush ativo após cada
// write pra que client receba incremental (não em batch ao fim).
//
// Erros pré-stream (search, rerank, setup LLM) viram 4xx/5xx normais.
// Erros mid-stream viram event:error no SSE (com 200 OK headers já enviados).
func QueryStreamHandler(svc *query.Service, pool *pgxpool.Pool) http.HandlerFunc {
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

		// Quota mensal de queries — ANTES de abrir o SSE (402 ainda é possível).
		if !checkQueryQuota(w, r, pool, orgID) {
			return
		}

		// Atribui a query à IA (relatório/billing por agent). Best-effort.
		var agentID int64
		if req.Agent != "" {
			agentID, _ = resolveAgentID(r.Context(), pool, orgID, req.Agent)
		}

		flusher, ok := w.(http.Flusher)
		if !ok {
			writeError(w, http.StatusInternalServerError, "streaming não suportado pelo writer")
			return
		}

		events, err := svc.QueryStream(r.Context(), orgID, query.Options{
			Question: req.Question,
			Limit:    req.Limit,
			Mode:     req.Mode,
			Domain:   req.Domain,
			MultiHop: req.MultiHop,
		})
		if err != nil {
			if errors.Is(err, llm.ErrStreamNotSupported) {
				writeError(w, http.StatusServiceUnavailable, "stream não suportado pelo provider LLM")
				return
			}
			writeInternalError(w, "query stream", err)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		// Captura o usage do evento done pra gravar o metering ao fim — o stream
		// hoje não metrificava (relatório/billing cegos pro SSE).
		var usage *query.Usage
		for evt := range events {
			if evt.Kind == query.StreamEventDone && evt.Usage != nil {
				usage = evt.Usage
			}
			data, err := json.Marshal(evt)
			if err != nil {
				_, _ = fmt.Fprintf(w, "event: error\ndata: {\"error\":\"marshal: %s\"}\n\n", err.Error())
				flusher.Flush()
				return
			}
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", evt.Kind, data)
			flusher.Flush()
		}

		// Metering: 1 evento de query (mesma métrica do /v1/query, via=stream).
		// WithoutCancel: client que desconecta mid-stream mata o r.Context() —
		// sem isso o registro do ledger se perde (org_usage conta, ledger não).
		meta := map[string]any{"via": "stream"}
		provider, model, latency := "", "", 0
		if usage != nil {
			provider, model, latency = usage.LLMProvider, usage.LLMModel, usage.LatencyMs
			meta["tokens_in"] = usage.TokensIn
			meta["tokens_out"] = usage.TokensOut
			meta["reranked"] = usage.Reranked
		}
		recordUsage(context.WithoutCancel(r.Context()), pool, orgID, agentID, "query", provider, model, latency, meta)
	}
}
