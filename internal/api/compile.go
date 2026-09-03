package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nexusyn/engine/internal/core/compile"
	"github.com/nexusyn/engine/internal/job"
	"github.com/nexusyn/engine/internal/provider/llm"
	"github.com/nexusyn/engine/internal/tenant"
)

// compileBatchSize — fontes processadas por chamada de LLM. Pequeno de propósito:
// JSON de saída grande trunca (limite de tokens) → parse falha. Lotes pequenos
// mantêm cada saída dentro do MaxTokens.
const compileBatchSize = 3

// compileRequest — body opcional: quais alvos gerar. Default: os 4.
type compileRequest struct {
	Targets []string `json:"targets"`
}

// CompileHandler — POST /v1/compile: lê memory+knowledge da org e DESTILA, POR
// LOTES, em páginas wiki/lesson/decision/error (upsert por slug + re-chunk).
// Falha de um lote/alvo é ignorada (coletada em "failures"), não derruba o resto.
func CompileHandler(pool *pgxpool.Pool, provider llm.Provider) http.HandlerFunc {
	insertClient, _ := job.NewInsertOnlyClient(pool)

	return func(w http.ResponseWriter, r *http.Request) {
		orgID, err := tenant.OrgIDFromContext(r.Context())
		if err != nil {
			writeError(w, http.StatusUnauthorized, "no tenant in context")
			return
		}
		if provider == nil {
			writeError(w, http.StatusServiceUnavailable, "LLM not configured")
			return
		}

		// Alvos: body ou default os 4 (wiki primeiro — mais pesado). Body é OPCIONAL
		// aqui (decode error != body-too-large é ignorado por design, mantém os
		// defaults) — mas o cap de tamanho SEMPRE se aplica (NEX-001): um body
		// gigante ainda deve dar 413 limpo, não ser lido inteiro na RAM.
		targets := []string{"wiki", "lesson", "decision", "error"}
		var req compileRequest
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		if derr := json.NewDecoder(r.Body).Decode(&req); derr != nil {
			var maxErr *http.MaxBytesError
			if errors.As(derr, &maxErr) {
				_ = r.Body.Close()
				writeError(w, http.StatusRequestEntityTooLarge,
					fmt.Sprintf("request body too large (max %d bytes)", maxErr.Limit))
				return
			}
			// body ausente/JSON inválido → mantém os defaults (comportamento original)
		} else if len(req.Targets) > 0 {
			valid := []string{}
			for _, t := range req.Targets {
				if _, ok := compile.Targets[t]; ok {
					valid = append(valid, t)
				}
			}
			if len(valid) > 0 {
				targets = valid
			}
		}
		_ = r.Body.Close()

		// 1. Junta fontes: memory + knowledge (entrada crua/curada).
		var sources []compile.Doc
		err = tenant.RunWithTenantReadOnly(r.Context(), pool, orgID, func(tx pgx.Tx) error {
			rows, e := tx.Query(r.Context(),
				`SELECT id, title, content, COALESCE(project, '') FROM pages
				 WHERE organization_id = $1 AND valid_to IS NULL
				   AND domain IN ('memory','knowledge')
				 ORDER BY created_at DESC LIMIT 200`, orgID)
			if e != nil {
				return e
			}
			defer rows.Close()
			for rows.Next() {
				var id int64
				var t, c, proj string
				if e := rows.Scan(&id, &t, &c, &proj); e != nil {
					return e
				}
				sources = append(sources, compile.Doc{ID: id, Title: t, Content: c, Project: proj})
			}
			return rows.Err()
		})
		if err != nil {
			writeInternalError(w, "compile gather", err)
			return
		}
		if len(sources) == 0 {
			writeError(w, http.StatusBadRequest, "nada pra compilar: ingira memórias/knowledge primeiro")
			return
		}

		// 2. Roda o compile em BACKGROUND (LLM é lento; ~N lotes × alvos não cabe numa
		// request síncrona — o cliente desconecta e cancela). Goroutine destacada com
		// contexto próprio (sobrevive ao return). Idempotente (UPSERT) → re-run seguro.
		// O núcleo (lotes + pacing/retry + persist + grafo) vive em job.RunCompile,
		// compartilhado com o auto-compile periódico (worker).
		go func() {
			bg, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
			defer cancel()
			// Conteúdo derivado é atribuído a um agente (não-órfão). find-or-create;
			// 0 → NULL no INSERT se resolve falhar.
			agentID, _ := resolveAgentID(bg, pool, orgID, "claude")
			counts, failures := job.RunCompile(bg, pool, insertClient, provider, orgID, agentID, sources, targets)
			slog.Info("compile done", "org_id", orgID, "counts", counts, "sources", len(sources), "failures", len(failures))
			if len(failures) > 0 {
				slog.Warn("compile failures", "org_id", orgID, "list", failures)
			}
		}()

		writeJSON(w, http.StatusAccepted, map[string]any{
			"status":  "started",
			"message": "compile rodando em background — wiki/lesson/decision/error vão aparecendo conforme processam",
			"sources": len(sources),
			"targets": targets,
		})
	}
}

// gatherDomainDocs/persistPages + o loop de lotes (com pacing/retry) viveram aqui;
// foram movidos pra internal/job (compile_run.go) pra serem compartilhados com o
// auto-compile periódico do worker. CompileHandler agora só reúne fontes e delega
// a job.RunCompile.
