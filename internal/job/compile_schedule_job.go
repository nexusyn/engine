package job

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/nexusyn/engine/internal/core/compile"
	"github.com/nexusyn/engine/internal/metering"
	"github.com/nexusyn/engine/internal/provider/llm"
	"github.com/nexusyn/engine/internal/tenant"
)

// compileAllTargets — as 4 lentes geradas pelo auto-compile (mesma ordem do handler).
var compileAllTargets = []string{"wiki", "lesson", "decision", "error"}

// ─────────────────────────────────────────────────────────────────────────────
// Tick do scheduler: enumera orgs com fonte nova desde o watermark e enfileira
// 1 CompileOrgJob por org (cap MaxOrgsPerTick). Disparado pelo PeriodicJob.
// ─────────────────────────────────────────────────────────────────────────────

type ScheduleCompileArgs struct{}

func (ScheduleCompileArgs) Kind() string { return "schedule_compile" }

func (ScheduleCompileArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       "default",
		MaxAttempts: 2,
		// Dedup: se o tick anterior ainda roda quando o periódico dispara, não acumula.
		UniqueOpts: river.UniqueOpts{ByPeriod: 5 * time.Minute},
	}
}

type ScheduleCompileWorker struct {
	river.WorkerDefaults[ScheduleCompileArgs]
	pool    *pgxpool.Pool
	maxOrgs int
}

func NewScheduleCompileWorker(pool *pgxpool.Pool, maxOrgs int) *ScheduleCompileWorker {
	if maxOrgs <= 0 {
		maxOrgs = 1
	}
	return &ScheduleCompileWorker{pool: pool, maxOrgs: maxOrgs}
}

func (w *ScheduleCompileWorker) Work(ctx context.Context, job *river.Job[ScheduleCompileArgs]) error {
	orgs, err := orgsNeedingCompile(ctx, w.pool, w.maxOrgs)
	if err != nil {
		return fmt.Errorf("schedule_compile: list orgs: %w", err)
	}
	if len(orgs) == 0 {
		slog.Info("schedule_compile: nothing to compile", "job_id", job.ID)
		return nil
	}
	client, err := NewInsertOnlyClient(w.pool)
	if err != nil {
		return fmt.Errorf("schedule_compile: insert client: %w", err)
	}
	var enqueued int
	for _, orgID := range orgs {
		if eerr := EnqueueCompileOrg(ctx, client, orgID); eerr != nil {
			slog.Warn("schedule_compile: enqueue failed", "org_id", orgID, "err", eerr)
			continue
		}
		enqueued++
	}
	slog.Info("schedule_compile done", "job_id", job.ID, "orgs", len(orgs), "enqueued", enqueued)
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Compile de uma org: pega só as fontes novas desde o watermark (máx maxItems,
// mais antigas primeiro → avança cronologicamente), destila e avança o watermark.
// ─────────────────────────────────────────────────────────────────────────────

type CompileOrgArgs struct {
	OrgID int64 `json:"org_id"`
}

func (CompileOrgArgs) Kind() string { return "compile_org" }

func (CompileOrgArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       "compile",
		MaxAttempts: 2,
		// Dedup por org: dois ticks não rodam a mesma org em paralelo.
		UniqueOpts: river.UniqueOpts{ByArgs: true, ByPeriod: 30 * time.Minute},
	}
}

type CompileOrgWorker struct {
	river.WorkerDefaults[CompileOrgArgs]
	pool      *pgxpool.Pool
	provider  llm.Provider
	maxItems  int
	agentSlug string
}

func NewCompileOrgWorker(pool *pgxpool.Pool, provider llm.Provider, maxItems int, agentSlug string) *CompileOrgWorker {
	if maxItems <= 0 {
		maxItems = 30
	}
	if agentSlug == "" {
		agentSlug = "claude"
	}
	return &CompileOrgWorker{pool: pool, provider: provider, maxItems: maxItems, agentSlug: agentSlug}
}

// Timeout sobrescreve o DEFAULT de 60s do River. O compile de uma org roda muitos
// lotes de LLM (até maxItems/3 × 4 lentes, com pacing 3s + retry/backoff) → leva
// minutos. Sem este override, os 60s cancelam o ctx no meio: todas as chamadas
// seguintes dão "context deadline exceeded" (falhas em cascata) e o watermark não
// grava. 30min cobre folgado uma rodada de 30 itens.
func (w *CompileOrgWorker) Timeout(*river.Job[CompileOrgArgs]) time.Duration {
	return 30 * time.Minute
}

func (w *CompileOrgWorker) Work(ctx context.Context, job *river.Job[CompileOrgArgs]) error {
	orgID := job.Args.OrgID

	since, err := getCompileWatermark(ctx, w.pool, orgID)
	if err != nil {
		return fmt.Errorf("compile_org: watermark org=%d: %w", orgID, err)
	}
	sources, maxTS, err := gatherNewSources(ctx, w.pool, orgID, since, w.maxItems)
	if err != nil {
		return fmt.Errorf("compile_org: gather org=%d: %w", orgID, err)
	}
	if len(sources) == 0 {
		slog.Info("compile_org: nothing new", "org_id", orgID)
		return nil
	}

	agentID, _ := metering.ResolveAgent(ctx, w.pool, orgID, w.agentSlug)
	insertClient, err := NewInsertOnlyClient(w.pool)
	if err != nil {
		return fmt.Errorf("compile_org: insert client: %w", err)
	}

	counts, failures := RunCompile(ctx, w.pool, insertClient, w.provider, orgID, agentID, sources, compileAllTargets)

	// Avança o watermark mesmo com falhas de lote (pacing/retry já tratam o
	// transitório) — garante progresso e evita loop preso no mesmo lote. Contexto
	// destacado (WithoutCancel) pra que um cancelamento de fim de rodada não perca
	// o progresso já feito.
	wmCtx := context.WithoutCancel(ctx)
	if serr := setCompileWatermark(wmCtx, w.pool, orgID, maxTS); serr != nil {
		slog.Warn("compile_org: advance watermark failed", "org_id", orgID, "err", serr)
	}
	slog.Info("compile_org done",
		"org_id", orgID, "items", len(sources), "counts", counts,
		"failures", len(failures), "watermark", maxTS)
	return nil
}

// EnqueueCompileOrg enfileira o compile de uma org (idempotente via UniqueOpts).
func EnqueueCompileOrg(ctx context.Context, client *river.Client[pgx.Tx], orgID int64) error {
	_, err := client.Insert(ctx, CompileOrgArgs{OrgID: orgID}, nil)
	return err
}

// ─────────────────────────────────────────────────────────────────────────────
// Wiring + helpers
// ─────────────────────────────────────────────────────────────────────────────

// RegisterCompileWorkers anexa o scheduler + o worker per-org. Só faz sentido com
// um provider de LLM (MiniMax-M2.7) configurado.
func RegisterCompileWorkers(workers *river.Workers, pool *pgxpool.Pool, provider llm.Provider, maxItems, maxOrgs int, agentSlug string) error {
	if err := river.AddWorkerSafely(workers, NewScheduleCompileWorker(pool, maxOrgs)); err != nil {
		return err
	}
	return river.AddWorkerSafely(workers, NewCompileOrgWorker(pool, provider, maxItems, agentSlug))
}

// NewCompilePeriodicJob retorna o PeriodicJob (anexar ao river.Config). RunOnStart
// false: compile é caro — não dispara a cada restart do worker, espera 1 intervalo.
func NewCompilePeriodicJob(interval time.Duration) *river.PeriodicJob {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	return river.NewPeriodicJob(
		river.PeriodicInterval(interval),
		func() (river.JobArgs, *river.InsertOpts) {
			return ScheduleCompileArgs{}, nil
		},
		&river.PeriodicJobOpts{RunOnStart: false},
	)
}

// orgsNeedingCompile lista (cross-org, via SECURITY DEFINER) orgs com fonte nova.
func orgsNeedingCompile(ctx context.Context, pool *pgxpool.Pool, limit int) ([]int64, error) {
	rows, err := pool.Query(ctx, `SELECT organization_id FROM list_orgs_needing_compile($1)`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var orgs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		orgs = append(orgs, id)
	}
	return orgs, rows.Err()
}

func getCompileWatermark(ctx context.Context, pool *pgxpool.Pool, orgID int64) (time.Time, error) {
	var ts time.Time
	err := pool.QueryRow(ctx, `SELECT get_compile_watermark($1)`, orgID).Scan(&ts)
	return ts, err
}

func setCompileWatermark(ctx context.Context, pool *pgxpool.Pool, orgID int64, ts time.Time) error {
	_, err := pool.Exec(ctx, `SELECT set_compile_watermark($1, $2)`, orgID, ts)
	return err
}

// gatherNewSources lê as fontes (memory/knowledge) vivas criadas DEPOIS de `since`,
// mais antigas primeiro, até `limit`. Retorna os Docs + o created_at máximo (novo
// watermark). RLS via RunWithTenantReadOnly.
func gatherNewSources(ctx context.Context, pool *pgxpool.Pool, orgID int64, since time.Time, limit int) ([]compile.Doc, time.Time, error) {
	var docs []compile.Doc
	maxTS := since
	err := tenant.RunWithTenantReadOnly(ctx, pool, orgID, func(tx pgx.Tx) error {
		rows, e := tx.Query(ctx,
			`SELECT id, title, content, COALESCE(project, ''), created_at FROM pages
			 WHERE organization_id = $1 AND valid_to IS NULL
			   AND domain IN ('memory','knowledge') AND created_at > $2
			 ORDER BY created_at ASC LIMIT $3`, orgID, since, limit)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			var t, c, proj string
			var ts time.Time
			if e := rows.Scan(&id, &t, &c, &proj, &ts); e != nil {
				return e
			}
			docs = append(docs, compile.Doc{ID: id, Title: t, Content: c, Project: proj})
			if ts.After(maxTS) {
				maxTS = ts
			}
		}
		return rows.Err()
	})
	return docs, maxTS, err
}
