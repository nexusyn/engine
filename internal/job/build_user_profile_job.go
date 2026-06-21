package job

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/nexusyn/engine/internal/core/profile"
	"github.com/nexusyn/engine/internal/provider/llm"
)

// BuildUserProfileArgs identifica a org-alvo. Job é granular (1 org por job)
// pra que retry/failure de uma org não afete outras.
type BuildUserProfileArgs struct {
	OrganizationID int64 `json:"organization_id"`
}

func (BuildUserProfileArgs) Kind() string { return "build_user_profile" }

func (BuildUserProfileArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       "default",
		MaxAttempts: 3,
		// Dedup natural: cada nova insert sobrescreve insertOpts mas a key
		// `UniqueOpts.ByArgs=true` evita duplicar jobs in-flight pra mesma org.
		UniqueOpts: river.UniqueOpts{
			ByArgs:   true,
			ByPeriod: 5 * time.Minute,
		},
	}
}

// EnqueueBuildUserProfile é o helper chamado pelo scheduler (PeriodicJob).
func EnqueueBuildUserProfile(ctx context.Context, client *river.Client[pgx.Tx], orgID int64) error {
	if _, err := client.Insert(ctx, BuildUserProfileArgs{OrganizationID: orgID}, &river.InsertOpts{}); err != nil {
		return fmt.Errorf("enqueue build_user_profile: %w", err)
	}
	return nil
}

// BuildUserProfileWorker chama profile.Build pra sintetizar e UPSERT o profile
// da org. Idempotente — UPSERT é seguro pra rodar com qualquer frequência.
type BuildUserProfileWorker struct {
	river.WorkerDefaults[BuildUserProfileArgs]
	pool *pgxpool.Pool
	llm  llm.Provider
}

// Timeout override (default River = 1min). Synthesize LLM call pode demorar
// 30-60s em Cloud e 5-15min em Ollama CPU 14B. 15min cobre ambos com folga.
func (w *BuildUserProfileWorker) Timeout(_ *river.Job[BuildUserProfileArgs]) time.Duration {
	return 15 * time.Minute
}

func NewBuildUserProfileWorker(pool *pgxpool.Pool, p llm.Provider) *BuildUserProfileWorker {
	return &BuildUserProfileWorker{pool: pool, llm: p}
}

// RegisterBuildUserProfileWorker é chamado pelo wiring SE llm provider disponível.
// Sem LLM, jobs ficam retryable até worker certo aparecer (mesmo padrão
// do ExtractEntitiesWorker).
func RegisterBuildUserProfileWorker(workers *river.Workers, pool *pgxpool.Pool, p llm.Provider) error {
	if p == nil {
		return errors.New("BuildUserProfileWorker: llm provider nil — não registrado")
	}
	return river.AddWorkerSafely(workers, NewBuildUserProfileWorker(pool, p))
}

func (w *BuildUserProfileWorker) Work(ctx context.Context, job *river.Job[BuildUserProfileArgs]) error {
	args := job.Args
	if args.OrganizationID <= 0 {
		return fmt.Errorf("BuildUserProfileJob: OrganizationID inválido: %d", args.OrganizationID)
	}

	start := time.Now()
	p, err := profile.Build(ctx, w.pool, w.llm, args.OrganizationID)
	if err != nil {
		return fmt.Errorf("build_user_profile: org %d: %w", args.OrganizationID, err)
	}

	slog.Info("build_user_profile done",
		"job_id", job.ID,
		"org_id", args.OrganizationID,
		"preferences_count", p.PreferencesCount,
		"lessons_count", p.LessonsCount,
		"content_len", len(p.Content),
		"llm_provider", w.llm.Name(),
		"latency_ms", time.Since(start).Milliseconds(),
	)
	return nil
}
