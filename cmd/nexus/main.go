// Command nexus é o binário único do NEXUS.
//
// Subcommands:
//
//	nexus serve              # HTTP API + (futuramente) MCP server
//	nexus serve --worker     # roda só workers River (background jobs)
//	nexus migrate up         # roda migrations pendentes
//	nexus migrate down       # reverte última migration
//	nexus migrate status     # lista status
//	nexus bench longmemeval  # bench mode
//	nexus admin reset        # truncate tabelas (test only)
//	nexus health-check       # liveness probe (usado pelo Docker HEALTHCHECK)
//
// Variáveis injetadas pelo Makefile via ldflags:
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/nexusyn/engine/internal/api"
	"github.com/nexusyn/engine/internal/auth"
	"github.com/nexusyn/engine/internal/config"
	"github.com/nexusyn/engine/internal/core/query"
	"github.com/nexusyn/engine/internal/core/search"
	"github.com/nexusyn/engine/internal/job"
	"github.com/nexusyn/engine/internal/mcp"
	"github.com/nexusyn/engine/internal/metering"
	"github.com/nexusyn/engine/internal/modelresolver"
	"github.com/nexusyn/engine/internal/provider/embed"
	"github.com/nexusyn/engine/internal/provider/llm"
	"github.com/nexusyn/engine/internal/provider/rerank"
	"github.com/nexusyn/engine/internal/reqlog"
	"github.com/nexusyn/engine/internal/secret"
	"github.com/nexusyn/engine/internal/storage"
)

// Injetadas pelo Makefile/Docker build via -ldflags.
var (
	version   = "dev"
	commit    = "unknown"
	buildTime = "unknown"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(2)
	}

	// slog padrão (será configurado via config.Log na inicialização real)
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "serve":
		if err := runServe(args); err != nil {
			slog.Error("serve failed", "err", err)
			os.Exit(1)
		}
	case "health-check":
		// Liveness probe usado pelo HEALTHCHECK do Dockerfile.
		// Faz HTTP GET no próprio servidor — exit 0 se ok, 1 se falha.
		if err := runHealthCheck(); err != nil {
			os.Exit(1)
		}
	case "migrate":
		if err := runMigrate(args); err != nil {
			slog.Error("migrate failed", "err", err)
			os.Exit(1)
		}
	case "reindex-dates":
		if err := runReindexDates(args); err != nil {
			slog.Error("reindex-dates failed", "err", err)
			os.Exit(1)
		}
	case "reextract":
		if err := runReextract(args); err != nil {
			slog.Error("reextract failed", "err", err)
			os.Exit(1)
		}
	case "dedup-entities":
		if err := runDedupEntities(args); err != nil {
			slog.Error("dedup-entities failed", "err", err)
			os.Exit(1)
		}
	case "bench":
		fmt.Println("bench: ainda não implementado — Semana 7 do roadmap")
		os.Exit(2)
	case "admin":
		if err := runAdmin(args); err != nil {
			slog.Error("admin failed", "err", err)
			os.Exit(1)
		}
	case "version", "--version", "-v":
		fmt.Printf("nexus %s (commit %s, built %s)\n", version, commit, buildTime)
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "comando desconhecido: %s\n\n", cmd)
		printUsage()
		os.Exit(2)
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, `NEXUS — Memória persistente para agentes de IA

Uso:
  nexus <command> [flags]

Comandos:
  serve              Inicia HTTP API (porta 8044 por default)
  health-check       Liveness probe (usado pelo Docker HEALTHCHECK)
  migrate            Migrations DB (subcommands: up, down, status) [TODO]
  bench longmemeval  Roda bench LongMemEval [TODO]
  admin reset        Trunca tabelas (test only) [TODO]
  version            Imprime versão
  help               Esta mensagem

Configuração:
  Todos os campos vêm de variáveis de ambiente. Veja .env.example.

Docs:
  https://github.com/nexusyn/engine/blob/main/docs/`)
}

// runServe inicia o servidor HTTP ou modo worker.
// Flags:
//
//	--worker   roda apenas workers River (sem HTTP server)
//	(default)  roda HTTP server + insert-only river client (enqueue de jobs)
func runServe(args []string) error {
	workerMode := false
	for _, a := range args {
		if a == "--worker" {
			workerMode = true
		}
	}
	if workerMode {
		return runWorker()
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	slog.Info("starting nexus",
		"version", version,
		"commit", commit,
		"port", cfg.HTTP.Port,
	)

	ctx := context.Background()

	// Pool Postgres
	pool, err := storage.NewPool(ctx, cfg.DB)
	if err != nil {
		return fmt.Errorf("db pool: %w", err)
	}
	defer pool.Close()
	slog.Info("db pool ready", "max_conns", cfg.DB.MaxConns)

	// Carrega o flag de enforcement de cota do DB (persiste o toggle do admin entre
	// restarts; sem linha = default do .env, OFF no beta).
	metering.RefreshEnforced(ctx, pool)
	slog.Info("enforcement de cota", "enforced", metering.Enforced())

	// Cipher pra config de modelo global (API keys cifradas). Opcional: sem
	// CONFIG_ENC_KEY, os endpoints /v1/admin/model-config respondem 503.
	cfgCipher, cerr := secret.FromEnv()
	if cerr != nil {
		slog.Warn("config cipher off — /v1/admin/model-config indisponível (set CONFIG_ENC_KEY)", "err", cerr)
		cfgCipher = nil
	}

	// Embed provider (opcional — search funciona com FTS-only se ausente).
	// Gate: Jina (default e único provider) precisa da sua key.
	var embedProvider embed.Provider
	embedConfigured := (cfg.Embed.Provider == "jina" || cfg.Embed.Provider == "") && cfg.Embed.Jina.APIKey != ""
	if embedConfigured {
		embedProvider, err = embed.Factory(cfg.Embed)
		if err != nil {
			return fmt.Errorf("embed provider: %w", err)
		}
		slog.Info("embed provider ready",
			"provider", embedProvider.Name(),
			"model", embedProvider.Model(),
			"dim", embedProvider.Dim(),
		)
	} else {
		slog.Warn("embed provider não configurado — /v1/search só fará FTS")
	}

	// Search service
	searchSvc := search.NewService(pool, embedProvider)

	// Rerank provider (opcional)
	var rerankProvider rerank.Provider
	if cfg.Rerank.Driver != "" && cfg.Rerank.Driver != "none" {
		rerankProvider, err = rerank.Factory(cfg.Rerank, cfg.Embed)
		if err != nil {
			return fmt.Errorf("rerank: %w", err)
		}
		if rerankProvider != nil {
			slog.Info("rerank ready", "provider", rerankProvider.Name(), "model", rerankProvider.Model())
		}
	}

	// LLM provider (opcional — /v1/query desabilitado se ausente)
	var llmProvider llm.Provider
	llmProvider, llmErr := llm.Factory(cfg.LLM)
	if llmErr != nil {
		slog.Warn("llm provider não configurado — /v1/query indisponível", "err", llmErr)
	} else {
		slog.Info("llm provider ready", "provider", llmProvider.Name(), "model", llmProvider.Model())
	}

	// Compile usa MiniMax-M2.7 (mais rápido que o M3 primário): o compile faz MUITOS
	// lotes seguidos e o M3 degrada/throttle no meio (timeout em cascata, fallbacks
	// mortos). M2.7 completa antes. Fallback pro primário se não construir.
	compileProvider := llmProvider
	if p, e := llm.BuildFor(cfg.LLM, "minimax", "MiniMax-M2.7"); e == nil && p != nil {
		compileProvider = p
		slog.Info("compile provider ready", "model", "MiniMax-M2.7")
	}

	// Juiz pro /v1/eval — independente do modelo testado (prefere anthropic).
	var evalJudge llm.Provider
	for _, jp := range []string{"anthropic", "gemini"} {
		if p, jerr := llm.BuildFor(cfg.LLM, jp, ""); jerr == nil && p != nil {
			evalJudge = p
			break
		}
	}
	if evalJudge == nil {
		evalJudge = llmProvider // fallback: usa o primário como juiz
	}

	// Resolver compartilhado: resolve generation/extraction/embed/rerank ao vivo
	// da config global (platform_model_config) > .env. O MESMO resolver vai pro
	// search (embed) e query (gen+rerank) → consistência garantida.
	resolver := modelresolver.New(pool, cfgCipher, *cfg, llmProvider, llmProvider, embedProvider, rerankProvider)
	searchSvc.EnableResolver(resolver)

	// Query service (precisa de search + llm; rerank opcional)
	// Sprint 1.5: pool habilita preference injection no /v1/query
	var querySvc *query.Service
	if llmProvider != nil {
		querySvc = query.NewServiceWithPool(searchSvc, llmProvider, rerankProvider, pool)
		querySvc.EnableOrgModels(cfg.LLM)      // override de modelo por org (Fase 0)
		querySvc.EnableModelResolver(resolver) // config global ao vivo (gen + rerank)
	}

	r := chi.NewRouter()

	// Middlewares globais
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(cfg.HTTP.WriteTimeout))

	// Públicos (sem auth, sem tenancy)
	r.Get("/health", api.HealthHandler(version, commit))
	r.Get("/ready", api.ReadyHandler(pool))

	// /v1/* requer Bearer token + tenancy auto-bound via context
	r.Route("/v1", func(r chi.Router) {
		r.Use(auth.Middleware(pool))
		r.Use(reqlog.Middleware) // após auth: log de atividade por org (tela Requests)
		// Rate limit por org (org_limits.max_rps, token bucket in-memory).
		// Gated por NEXUS_ENFORCE_LIMITS — OFF = passthrough.
		r.Use(api.NewRateLimiter(pool).Middleware)

		// Stub temporário: GET /v1/me — útil pra smoke-test do auth flow
		r.Get("/me", api.MeHandler())

		// Day 6: ingest assíncrono via River
		r.Post("/ingest", api.IngestHandler(pool))

		// Day 9: search híbrido (vector + FTS + RRF). Conta na quota de queries.
		r.Post("/search", api.SearchHandler(searchSvc, pool))

		// Day 10: query grounded (search + rerank + LLM)
		// Day 13: streaming SSE (POST /v1/query/stream)
		if querySvc != nil {
			r.Post("/query", api.QueryHandler(querySvc, pool))
			r.Post("/query/stream", api.QueryStreamHandler(querySvc, pool))
		}

		// Day 16/17: graph traversal multi-hop + temporal as_of
		r.Get("/entities", api.EntitiesListHandler(pool))
		r.Get("/entities/{slug}/related", api.EntityRelatedHandler(pool))
		r.Get("/entities/{slug}/memories", api.EntityMemoriesHandler(pool))
		// Memory Graph (visualização): subgrafo inicial (god-nodes + arestas).
		r.Get("/graph", api.GraphHandler(pool))

		// Fase 0 dashboard: config de modelo por etapa (override per-org do
		// LLMConfig global). GET lista as etapas configuradas; PUT grava uma.
		// ADMIN-ONLY: config de provider/modelo é do operador, não do cliente.
		// RequireAbility("admin") → token precisa de "admin" (ou "*"); keys de
		// cliente nascem restritas → 403. Defesa real, cobre API direta.
		r.With(auth.RequireAbility("admin")).Get("/config/models", api.ModelConfigGetHandler(pool))
		r.With(auth.RequireAbility("admin")).Put("/config/models", api.ModelConfigPutHandler(pool))

		// Config de modelo GLOBAL do operador (admin-only): provider + key cifrada
		// + modelo por etapa, com teste de conexão. NÃO mexe em org_model_config.
		// cipher nil (CONFIG_ENC_KEY ausente) → handlers respondem 503 informativo.
		r.With(auth.RequireAbility("admin")).Get("/admin/model-config", api.PlatformModelConfigGetHandler(pool, cfgCipher, *cfg))
		r.With(auth.RequireAbility("admin")).Put("/admin/model-config", api.PlatformModelConfigPutHandler(pool, cfgCipher))
		r.With(auth.RequireAbility("admin")).Post("/admin/model-config/test", api.PlatformModelConfigTestHandler(pool, cfgCipher, *cfg))

		// Re-index de embeddings (troca de modelo de embed): re-embedda todos os
		// chunks com o modelo corrente. Admin-only.
		r.With(auth.RequireAbility("admin")).Post("/admin/reindex-embeddings", api.ReindexEmbeddingsHandler(pool, resolver))
		r.With(auth.RequireAbility("admin")).Get("/admin/reindex-status", api.ReindexStatusHandler(pool))

		// Control-plane: provisiona org nova + token bootstrap (signup self-serve
		// do console chama isto com o MASTER token). ADMIN-ONLY.
		r.With(auth.RequireAbility("admin")).Post("/admin/orgs", api.AdminCreateOrgHandler(pool))

		// Control-plane: grava a quota de plano da org (max_pages/queries/rps).
		// Chamado pelo console (SyncTenantPlan) ao mudar de plano. ADMIN-ONLY.
		r.With(auth.RequireAbility("admin")).Put("/admin/orgs/{id}/limits", api.AdminSetLimitsHandler(pool))

		// Control-plane: suspender/reativar org (billing). Chamado pelo console
		// (HandleStripeWebhook) quando a assinatura falha/cancela. ADMIN-ONLY. AUD-010.
		r.With(auth.RequireAbility("admin")).Patch("/admin/orgs/{id}/suspend", api.AdminSuspendHandler(pool, true))
		r.With(auth.RequireAbility("admin")).Patch("/admin/orgs/{id}/unsuspend", api.AdminSuspendHandler(pool, false))

		// Control-plane: flags globais. enforcement = toggle de cota (botão do admin).
		// BETA: OFF. ADMIN-ONLY.
		r.With(auth.RequireAbility("admin")).Get("/admin/settings", api.AdminSettingsGetHandler(pool))
		r.With(auth.RequireAbility("admin")).Put("/admin/settings/enforcement", api.AdminEnforcementPutHandler(pool))

		// Control-plane: visão agregada cross-org de cota vs consumo (todas as orgs).
		// Backing da página de calibração de cota do console. ADMIN-ONLY.
		r.With(auth.RequireAbility("admin")).Get("/admin/usage", api.AdminUsageHandler(pool))

		// Control-plane: tradução via IA (operador digita PT, IA preenche EN nos
		// comunicados). Usa o LLM de geração configurado. ADMIN-ONLY.
		r.With(auth.RequireAbility("admin")).Post("/admin/translate", api.AdminTranslateHandler(resolver))

		// Control-plane: purge IRREVERSÍVEL da org + todos os dados (cascade).
		// Chamado pelo console na exclusão de conta pós-carência (LGPD). ADMIN-ONLY.
		r.With(auth.RequireAbility("admin")).Delete("/admin/orgs/{id}", api.AdminDeleteOrgHandler(pool))

		// Dashboard: gestão de api_tokens (tela API Keys). GET lista (sem segredo),
		// POST cria (retorna o token uma vez), DELETE revoga.
		r.Get("/tokens", api.TokensListHandler(pool))
		r.Post("/tokens", api.TokenCreateHandler(pool))
		r.Delete("/tokens/{id}", api.TokenDeleteHandler(pool))

		// Dashboard: Usage & Billing (counts da org) + Memory Exports (dump JSON).
		r.Get("/usage", api.UsageHandler(pool))
		r.Get("/export", api.ExportHandler(pool))

		// Dashboard: browse de pages por domain (Wiki=wiki, Knowledge Base=knowledge).
		r.Get("/memories", api.MemoriesListHandler(pool))
		// Só os ids do filtro atual (domain+q) — backing do "select all" do dashboard.
		// Estática ANTES de /memories/{id} (evita {id}="ids").
		r.Get("/memories/ids", api.MemoryIDsHandler(pool))
		// Edição/curadoria de memória (foco Wiki/Knowledge): editar markdown
		// (re-embeda) + deletar (soft-delete + sai da busca). Data-plane (org-scoped).
		r.Get("/memories/{id}", api.MemoryGetHandler(pool))
		r.Put("/memories/{id}", api.MemoryUpdateHandler(pool))
		r.Delete("/memories/{id}", api.MemoryDeleteHandler(pool))

		// Compile "Karpathy": LLM lê memory+knowledge → sintetiza páginas wiki
		// interligadas (domain=wiki). knowledge=entrada, wiki=saída compilada.
		r.Post("/compile", api.CompileHandler(pool, compileProvider))

		// Dashboard: atividade recente da API (tela Requests).
		r.Get("/requests", api.RequestsHandler())

		// Dashboard: audit log (events). ?kind=guideline. → mudanças de guideline
		// (notificação ao dono). Tenant-scoped por RLS.
		r.Get("/events", api.EventsListHandler(pool))

		// Dashboard: CRUD de webhooks (config; entrega = futuro).
		r.Get("/webhooks", api.WebhooksListHandler(pool))
		r.Post("/webhooks", api.WebhookCreateHandler(pool))
		r.Delete("/webhooks/{id}", api.WebhookDeleteHandler(pool))

		// Fase 0: /v1/eval — mini-bench (12 casos) com o modelo resolvido da org
		// ("test performance" do dashboard). Síncrono, ~30-40s.
		if querySvc != nil && evalJudge != nil {
			// ADMIN-ONLY: bench embutido (caro) é ferramenta de operador.
			r.With(auth.RequireAbility("admin")).Post("/eval", api.EvalHandler(querySvc, evalJudge))
		}

		// MCP: add_memory/search_memory pra agentes (Claude Code/Cursor/Hermes/
		// openclaw). Atrás do auth.Middleware → org resolvida por token
		// (streamable HTTP, stateless, multi-tenant). Endpoint: /v1/mcp.
		if querySvc != nil {
			if ic, ierr := job.NewInsertOnlyClient(pool); ierr == nil {
				r.Handle("/mcp", mcp.Handler(mcp.Deps{QuerySvc: querySvc, Insert: ic, Pool: pool, Version: version}))
			} else {
				slog.Warn("mcp: insert client falhou — /v1/mcp indisponível", "err", ierr)
			}
		}
	})

	srv := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.HTTP.Port),
		Handler:      r,
		ReadTimeout:  cfg.HTTP.ReadTimeout,
		WriteTimeout: cfg.HTTP.WriteTimeout,
	}

	// Graceful shutdown via SIGTERM/SIGINT
	errCh := make(chan error, 1)
	go func() {
		slog.Info("HTTP server listening", "addr", srv.Addr)
		errCh <- srv.ListenAndServe()
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("server: %w", err)
		}
	case sig := <-sigCh:
		slog.Info("received signal, shutting down", "signal", sig.String())
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			return fmt.Errorf("graceful shutdown: %w", err)
		}
	}

	slog.Info("nexus stopped")
	return nil
}

// runHealthCheck faz GET em /health no localhost — usado pelo HEALTHCHECK do Docker.
// Exit 0 = saudável; exit 1 = não saudável.
func runHealthCheck() error {
	// Porta é lida do env (mesmo que serve usa)
	port := os.Getenv("HTTP_PORT")
	if port == "" {
		port = "8044"
	}
	client := http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://localhost:%s/health", port))
	if err != nil {
		fmt.Fprintf(os.Stderr, "health-check: %v\n", err)
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "health-check: HTTP %d\n", resp.StatusCode)
		return fmt.Errorf("unhealthy: HTTP %d", resp.StatusCode)
	}
	return nil
}
