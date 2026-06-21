package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/nexusyn/engine/internal/config"
	"github.com/nexusyn/engine/internal/job"
	"github.com/nexusyn/engine/internal/modelresolver"
	embedprov "github.com/nexusyn/engine/internal/provider/embed"
	"github.com/nexusyn/engine/internal/provider/llm"
	"github.com/nexusyn/engine/internal/secret"
	"github.com/nexusyn/engine/internal/storage"
)

// runWorker inicia o processo worker: pool de conn + River client com workers
// registrados + Start. Bloqueia até SIGTERM/SIGINT, então faz graceful shutdown.
//
// Use em deploy separado:
//
//	nexus serve --worker
//
// Em dev, pode rodar lado-a-lado com `nexus serve` em outro terminal.
// Em prod, container dedicado pra workers escala independentemente.
func runWorker() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	slog.Info("starting nexus worker", "version", version, "commit", commit)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Pool Postgres (compartilhado entre todos workers do client)
	pool, err := storage.NewPool(ctx, cfg.DB)
	if err != nil {
		return fmt.Errorf("db pool: %w", err)
	}
	defer pool.Close()

	// Cipher pra config de modelo global (resolve embed/extract ao vivo). nil = off.
	cfgCipher, cerr := secret.FromEnv()
	if cerr != nil {
		slog.Warn("worker: config cipher off (CONFIG_ENC_KEY ausente) — embed/extract usam só o .env", "err", cerr)
		cfgCipher = nil
	}

	// Registra workers core (sem deps externas)
	workers := river.NewWorkers()
	if err := job.RegisterCoreWorkers(workers, pool); err != nil {
		return fmt.Errorf("register core workers: %w", err)
	}

	// Registra EmbedBatchWorker se provider de embedding configurado.
	// Gate: Jina (default e único provider) precisa da sua key.
	embedConfigured := (cfg.Embed.Provider == "jina" || cfg.Embed.Provider == "") && cfg.Embed.Jina.APIKey != ""
	if embedConfigured {
		provider, perr := embedprov.Factory(cfg.Embed)
		if perr != nil {
			return fmt.Errorf("embed provider: %w", perr)
		}
		embedRes := modelresolver.New(pool, cfgCipher, *cfg, nil, nil, provider, nil)
		if err := job.RegisterEmbedWorker(workers, pool, provider, embedRes, cfg.Embed.BatchSize); err != nil {
			return fmt.Errorf("register embed worker: %w", err)
		}
		slog.Info("embed worker registered",
			"provider", provider.Name(),
			"model", provider.Model(),
			"dim", provider.Dim(),
			"batch_size", cfg.Embed.BatchSize,
		)
	} else {
		slog.Warn("embed provider not configured — EmbedBatch jobs ficarão pendentes (JINA_API_KEY ausente)")
	}

	// Registra ExtractEntities + BuildUserProfile + ScheduleProfileRebuild se
	// LLM provider configurado. Sem LLM, jobs ficam retryable até worker certo
	// aparecer.
	var profilePeriodic *river.PeriodicJob
	if llmProvider, lerr := llm.Factory(cfg.LLM); lerr == nil && llmProvider != nil {
		// Extractor pode usar um modelo dedicado (EXTRACT_PROVIDER/MODEL) —
		// barato/rápido pra o alto volume de ingest. Vazio → router global.
		extractLLM := llmProvider
		if cfg.LLM.ExtractProvider != "" {
			if p, berr := llm.BuildFor(cfg.LLM, cfg.LLM.ExtractProvider, cfg.LLM.ExtractModel); berr == nil && p != nil {
				extractLLM = p
			} else {
				slog.Warn("extract provider dedicado falhou, usando router global", "provider", cfg.LLM.ExtractProvider, "err", berr)
			}
		}
		extractRes := modelresolver.New(pool, cfgCipher, *cfg, nil, extractLLM, nil, nil)
		if err := job.RegisterExtractEntitiesWorker(workers, pool, extractLLM, extractRes); err != nil {
			return fmt.Errorf("register extract entities worker: %w", err)
		}
		slog.Info("extract entities worker registered",
			"llm_provider", extractLLM.Name(),
			"llm_model", extractLLM.Model(),
		)
		// Sprint 3.1 — user profile aggregation
		if err := job.RegisterBuildUserProfileWorker(workers, pool, llmProvider); err != nil {
			return fmt.Errorf("register build user profile worker: %w", err)
		}
		if err := job.RegisterScheduleProfileRebuildWorker(workers, pool); err != nil {
			return fmt.Errorf("register schedule profile rebuild worker: %w", err)
		}
		profilePeriodic = job.NewProfileRebuildPeriodicJob(0) // 0 → 1h default
		slog.Info("user profile workers registered (periodic rebuild every 1h)")
	} else {
		slog.Warn("llm provider not configured — ExtractEntities + BuildUserProfile jobs ficarão pendentes")
	}

	// Auto-compile periódico (opt-in: COMPILE_INTERVAL>0). Incremental por watermark,
	// teto de itens/org por rodada + teto de orgs/tick (anti-contenção da key MiniMax).
	// Usa MiniMax-M2.7 (mais rápido em muitos lotes; mesmo do compile HTTP).
	var compilePeriodic *river.PeriodicJob
	if cfg.Compile.Interval > 0 {
		if cp, e := llm.BuildFor(cfg.LLM, "minimax", "MiniMax-M2.7"); e == nil && cp != nil {
			if err := job.RegisterCompileWorkers(workers, pool, cp,
				cfg.Compile.MaxItemsPerRun, cfg.Compile.MaxOrgsPerTick, cfg.Compile.Agent); err != nil {
				return fmt.Errorf("register compile workers: %w", err)
			}
			compilePeriodic = job.NewCompilePeriodicJob(cfg.Compile.Interval)
			slog.Info("auto-compile workers registered",
				"interval", cfg.Compile.Interval,
				"max_items_per_run", cfg.Compile.MaxItemsPerRun,
				"max_orgs_per_tick", cfg.Compile.MaxOrgsPerTick,
				"agent", cfg.Compile.Agent)
		} else {
			slog.Warn("auto-compile ligado (COMPILE_INTERVAL) mas provider MiniMax-M2.7 indisponível — desligado", "err", e)
		}
	}

	periodics := make([]*river.PeriodicJob, 0, 2)
	if profilePeriodic != nil {
		periodics = append(periodics, profilePeriodic)
	}
	if compilePeriodic != nil {
		periodics = append(periodics, compilePeriodic)
	}
	var client *river.Client[pgx.Tx]
	client, err = job.NewClient(pool, workers, periodics...)
	if err != nil {
		return fmt.Errorf("river client: %w", err)
	}

	if err := client.Start(ctx); err != nil {
		return fmt.Errorf("river start: %w", err)
	}
	slog.Info("river workers started",
		"queues", []string{"default", "ingest", "embed", "compile"})

	// Aguarda sinal de shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	slog.Info("worker shutdown signal received", "signal", sig.String())

	// Graceful: aguarda jobs em flight terminarem
	if err := client.Stop(ctx); err != nil && !errors.Is(err, context.Canceled) {
		slog.Warn("river stop", "err", err)
	}

	slog.Info("nexus worker stopped")
	return nil
}
