// Package job contém workers River (background jobs).
//
// River = fila Postgres-backed transacional. Filosofia:
//   - Jobs são enfileirados na MESMA transação do business write (atomicidade)
//   - Workers consomem em paralelo, com retries automáticos + dead-letter
//   - Migration própria do River roda junto com nossas via cmd/nexus/migrate.go
//   - Multi-tenant: TODO worker DEVE receber org_id no args e usar RunWithTenant
package job

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

// MigrateUp roda as migrations do River na DB.
// Chamado por `nexus migrate up` (cmd/nexus/migrate.go) após goose.Up das nossas.
func MigrateUp(ctx context.Context, pool *pgxpool.Pool) error {
	driver := riverpgxv5.New(pool)
	migrator, err := rivermigrate.New(driver, nil)
	if err != nil {
		return fmt.Errorf("river migrator init: %w", err)
	}
	res, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil)
	if err != nil {
		return fmt.Errorf("river migrate up: %w", err)
	}
	for _, v := range res.Versions {
		slog.Info("river migration applied", "version", v.Version, "name", v.Name)
	}
	return nil
}

// NewClient constrói um river.Client com workers registrados.
// Use em modo `--worker`: client.Start(ctx) abre poll do banco e processa jobs.
// Em modo `serve` (HTTP), use NewInsertOnlyClient pra apenas enfileirar.
//
// periodicJobs opcional — passe nil ou slice vazio se não usar scheduler.
//
// Tipo paramétrico é *river.Client[pgx.Tx] porque usamos riverpgxv5 driver.
func NewClient(pool *pgxpool.Pool, workers *river.Workers, periodicJobs ...*river.PeriodicJob) (*river.Client[pgx.Tx], error) {
	cfg := &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: 10},
			// Sprint 1.6.3 — aumentado de 5→16. Bench LongMemEval ingere 4-5
			// sessions por item; com 5 workers cada item gargalava no extract
			// (Gemini ~9s/page). 16 workers paraleliza todos os items numa
			// rodada quase concurrent. Limite Gemini ~500 RPM/key (>30 RPS)
			// dá folga.
			"ingest": {MaxWorkers: 16},
			// embed também subiu 3→8 — Jina rate limit (env) cobre.
			// Workers do bench são gated por rate_limiter no Jina client.
			"embed":   {MaxWorkers: 8},
			"compile": {MaxWorkers: 2},
		},
		Workers:      workers,
		PeriodicJobs: periodicJobs,
		Logger:       slog.Default(),
	}

	client, err := river.NewClient(riverpgxv5.New(pool), cfg)
	if err != nil {
		return nil, fmt.Errorf("river client: %w", err)
	}
	return client, nil
}

// NewInsertOnlyClient cria um cliente apenas pra enfileirar jobs (não consumir).
// Usado pelo HTTP server — workers rodam em processo separado (--worker).
func NewInsertOnlyClient(pool *pgxpool.Pool) (*river.Client[pgx.Tx], error) {
	cfg := &river.Config{
		// Sem Queues registradas → não consome, apenas insere
		Logger: slog.Default(),
	}
	client, err := river.NewClient(riverpgxv5.New(pool), cfg)
	if err != nil {
		return nil, fmt.Errorf("river insert-only client: %w", err)
	}
	return client, nil
}

// RegisterCoreWorkers anexa workers que não precisam de providers externos.
// Ponto único pra adicionar workers core — mantém wiring centralizado.
func RegisterCoreWorkers(workers *river.Workers, pool *pgxpool.Pool) error {
	if err := river.AddWorkerSafely(workers, NewIngestWorker(pool)); err != nil {
		return fmt.Errorf("register IngestWorker: %w", err)
	}
	return nil
}
