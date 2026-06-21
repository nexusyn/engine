package job

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/nexusyn/engine/internal/core/profile"
)

// ScheduleProfileRebuildArgs não tem campos — esse job é o "tick" do scheduler.
// Disparado pelo River PeriodicJob (intervalo configurado em NewClient).
// Worker enumera orgs com facts e enfileira 1 BuildUserProfileJob por org.
type ScheduleProfileRebuildArgs struct{}

func (ScheduleProfileRebuildArgs) Kind() string { return "schedule_profile_rebuild" }

func (ScheduleProfileRebuildArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       "default",
		MaxAttempts: 2,
		// Dedup: se o tick anterior ainda estiver in-flight quando o periódico
		// dispara, evita acumular ticks.
		UniqueOpts: river.UniqueOpts{
			ByPeriod: 5 * time.Minute,
		},
	}
}

// ScheduleProfileRebuildWorker enumera orgs com preferences/lessons e enfileira
// 1 BuildUserProfileJob por org. Idempotente (BuildUserProfileJob faz UPSERT
// e tem dedup ByArgs).
type ScheduleProfileRebuildWorker struct {
	river.WorkerDefaults[ScheduleProfileRebuildArgs]
	pool *pgxpool.Pool
}

func NewScheduleProfileRebuildWorker(pool *pgxpool.Pool) *ScheduleProfileRebuildWorker {
	return &ScheduleProfileRebuildWorker{pool: pool}
}

func RegisterScheduleProfileRebuildWorker(workers *river.Workers, pool *pgxpool.Pool) error {
	return river.AddWorkerSafely(workers, NewScheduleProfileRebuildWorker(pool))
}

func (w *ScheduleProfileRebuildWorker) Work(ctx context.Context, job *river.Job[ScheduleProfileRebuildArgs]) error {
	orgs, err := profile.OrgsWithFacts(ctx, w.pool)
	if err != nil {
		return fmt.Errorf("schedule_profile_rebuild: list orgs: %w", err)
	}
	if len(orgs) == 0 {
		slog.Info("schedule_profile_rebuild: no orgs with facts", "job_id", job.ID)
		return nil
	}

	client, err := NewInsertOnlyClient(w.pool)
	if err != nil {
		return fmt.Errorf("schedule_profile_rebuild: insert client: %w", err)
	}

	var enqueued int
	for _, orgID := range orgs {
		if eerr := EnqueueBuildUserProfile(ctx, client, orgID); eerr != nil {
			slog.Warn("schedule_profile_rebuild: enqueue failed", "org_id", orgID, "err", eerr)
			continue
		}
		enqueued++
	}

	slog.Info("schedule_profile_rebuild done",
		"job_id", job.ID,
		"orgs_total", len(orgs),
		"jobs_enqueued", enqueued,
	)
	return nil
}

// NewProfileRebuildPeriodicJob retorna o PeriodicJob pra anexar ao river.Config.
// Padrão: 1× por hora, RunOnStart=true (faz primeiro tick imediato).
func NewProfileRebuildPeriodicJob(interval time.Duration) *river.PeriodicJob {
	if interval <= 0 {
		interval = 1 * time.Hour
	}
	return river.NewPeriodicJob(
		river.PeriodicInterval(interval),
		func() (river.JobArgs, *river.InsertOpts) {
			return ScheduleProfileRebuildArgs{}, nil
		},
		&river.PeriodicJobOpts{RunOnStart: true},
	)
}
