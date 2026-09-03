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

	"github.com/nexusyn/engine/internal/core/entities"
	"github.com/nexusyn/engine/internal/provider/llm"
	"github.com/nexusyn/engine/internal/tenant"
)

// ExtractEntitiesArgs identifica a page-alvo. Job é granular (1 page por job)
// pra que retry/failure sejam isolados.
type ExtractEntitiesArgs struct {
	OrganizationID int64 `json:"organization_id"`
	PageID         int64 `json:"page_id"`
}

func (ExtractEntitiesArgs) Kind() string { return "extract_entities" }

func (ExtractEntitiesArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       "default",
		MaxAttempts: 3,
	}
}

// EnqueueExtractEntities é o helper chamado pelo IngestWorker pra agendar
// extração após criar a page. 1 job por page (granular, retry isolado).
func EnqueueExtractEntities(ctx context.Context, client *river.Client[pgx.Tx], orgID, pageID int64) error {
	if _, err := client.Insert(ctx, ExtractEntitiesArgs{
		OrganizationID: orgID,
		PageID:         pageID,
	}, &river.InsertOpts{}); err != nil {
		return fmt.Errorf("enqueue extract entities: %w", err)
	}
	return nil
}

// ExtractEntitiesWorker chama LLM pra extrair entities/edges do page content
// e persiste. Idempotente: marca pages.entities_extracted_at no fim — se já
// foi processado, retorna sem fazer trabalho duplicado.
// extractResolver resolve o LLM de extração ao vivo (config global). Interface
// fina pra não acoplar o pacote job. Seguro trocar ao vivo (afeta ingests futuros).
type extractResolver interface {
	Extraction(ctx context.Context) llm.Provider
}

type ExtractEntitiesWorker struct {
	river.WorkerDefaults[ExtractEntitiesArgs]
	pool     *pgxpool.Pool
	llm      llm.Provider
	resolver extractResolver // opcional — resolve o extractor ao vivo
}

// extractor devolve o LLM corrente: resolver (config global) > .env.
func (w *ExtractEntitiesWorker) extractor(ctx context.Context) llm.Provider {
	if w.resolver != nil {
		if p := w.resolver.Extraction(ctx); p != nil {
			return p
		}
	}
	return w.llm
}

// Timeout override do default River (1min). Extract de entities pode demorar
// muito mais em LLMs locais (Ollama CPU 14B: 1-5min para gerar JSON longo).
// 15min cobre Ollama CPU + Anthropic/Gemini API com folga.
func (w *ExtractEntitiesWorker) Timeout(_ *river.Job[ExtractEntitiesArgs]) time.Duration {
	return 15 * time.Minute
}

func NewExtractEntitiesWorker(pool *pgxpool.Pool, p llm.Provider, resolver extractResolver) *ExtractEntitiesWorker {
	return &ExtractEntitiesWorker{pool: pool, llm: p, resolver: resolver}
}

// RegisterExtractEntitiesWorker é chamado pelo wiring SE llm provider disponível.
// Sem LLM, ExtractEntitiesJob não pode rodar — pula registro pra que jobs
// enfileirados fiquem em retryable até worker certo aparecer (ou nunca).
func RegisterExtractEntitiesWorker(workers *river.Workers, pool *pgxpool.Pool, p llm.Provider, resolver extractResolver) error {
	if p == nil {
		return errors.New("ExtractEntitiesWorker: llm provider nil — não registrado")
	}
	return river.AddWorkerSafely(workers, NewExtractEntitiesWorker(pool, p, resolver))
}

func (w *ExtractEntitiesWorker) Work(ctx context.Context, job *river.Job[ExtractEntitiesArgs]) error {
	args := job.Args
	if args.OrganizationID <= 0 || args.PageID <= 0 {
		return fmt.Errorf("ExtractEntitiesJob: args inválidos %+v", args)
	}

	start := time.Now()

	// 1. Lê content + verifica idempotência num único SELECT
	var content string
	var createdAt time.Time
	var alreadyProcessed bool
	err := tenant.RunWithTenantReadOnly(ctx, w.pool, args.OrganizationID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT content, created_at, (entities_extracted_at IS NOT NULL) AS done
			FROM pages WHERE id = $1
		`, args.PageID).Scan(&content, &createdAt, &alreadyProcessed)
	})
	if err != nil {
		return fmt.Errorf("extract_entities: select page %d: %w", args.PageID, err)
	}
	if alreadyProcessed {
		slog.Info("extract_entities: page já processada, skip",
			"page_id", args.PageID, "org_id", args.OrganizationID, "job_id", job.ID)
		return nil
	}
	if content == "" {
		// Marca como processada pra não retry
		_ = tenant.RunWithTenant(ctx, w.pool, args.OrganizationID, func(tx pgx.Tx) error {
			return entities.MarkPageProcessed(ctx, tx, args.PageID)
		})
		return nil
	}

	// 2. Chama LLM extractor (resolvido ao vivo: config global > .env)
	extractLLM := w.extractor(ctx)
	extracted, err := entities.Extract(ctx, extractLLM, content, createdAt)
	if err != nil {
		return fmt.Errorf("extract_entities: LLM page %d: %w", args.PageID, err)
	}

	// 3. Persiste (UPSERT entities + INSERT edges + UPDATE flag) na MESMA tx
	stats, err := entities.RunInTenantTx(ctx, w.pool, args.OrganizationID, args.PageID, extracted)
	if err != nil {
		return fmt.Errorf("extract_entities: persist page %d: %w", args.PageID, err)
	}

	slog.Info("extract_entities done",
		"job_id", job.ID,
		"page_id", args.PageID,
		"org_id", args.OrganizationID,
		"entities_inserted", stats.EntitiesInserted,
		"entities_existing", stats.EntitiesExisting,
		"edges_inserted", stats.EdgesInserted,
		"edges_superseded", stats.EdgesSuperseded,
		"llm_provider", extractLLM.Name(),
		"latency_ms", time.Since(start).Milliseconds(),
	)
	return nil
}
