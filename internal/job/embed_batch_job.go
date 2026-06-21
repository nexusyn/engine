package job

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/nexusyn/engine/internal/provider/embed"
)

// EmbedBatchArgs é praticamente vazio — o job é "drenagem" da queue de pages
// com embedding=NULL. Não precisa saber ORG_ID porque processa across-tenant
// via funções SECURITY DEFINER (drain_pending_chunks/update_chunk_embedding).
//
// SEGURANÇA (cross-tenant by design): o drain LÊ o content (texto) de chunks de
// qualquer tenant — mas SÓ os que estão com embedding=NULL (estado transiente do
// ingest), nunca content arbitrário já indexado. A escrita é escopada por
// chunk_id (update_chunk_embedding), então não há cross-write entre tenants. O
// content cru transita pro provider de embed (egress) — ver threat model.
//
// MaxAttempts alto (5) porque a chamada ao provider de embed pode falhar
// transitoriamente.
type EmbedBatchArgs struct {
	// BatchSize override opcional (default vem do config)
	BatchSize int `json:"batch_size,omitempty"`
}

func (EmbedBatchArgs) Kind() string { return "embed_batch" }

func (EmbedBatchArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       "embed",
		MaxAttempts: 5,
		// Dedup: só 1 EmbedBatch na fila/scheduled simultaneamente. Múltiplos
		// IngestJobs disparando enqueue só acumulam em 1.
		UniqueOpts: river.UniqueOpts{
			ByState: []rivertype.JobState{
				rivertype.JobStatePending,
				rivertype.JobStateAvailable,
				rivertype.JobStateScheduled,
				rivertype.JobStateRunning,
				rivertype.JobStateRetryable,
			},
		},
	}
}

// EmbedBatchWorker drena pages com embedding NULL e calcula via provider.
//
// Fluxo:
//  1. Lock advisory pra evitar 2 workers rodando ao mesmo tempo
//  2. SELECT N pages WHERE embedding IS NULL ORDER BY created_at
//  3. Chama provider.Embed(batch) — 1 round-trip pra N páginas
//  4. UPDATE pages SET embedding=$1 WHERE id=$2 (loop)
//  5. Se ainda há páginas pendentes, enqueue novo EmbedBatchJob (chain)
//
// embedResolver resolve o embed provider ao vivo (config global). Interface fina
// pra não acoplar o pacote job ao modelresolver. CRÍTICO: tem que ser o MESMO
// resolver do search.Service, senão chunks e query ficam em modelos diferentes.
type embedResolver interface {
	Embed(ctx context.Context) embed.Provider
}

type EmbedBatchWorker struct {
	river.WorkerDefaults[EmbedBatchArgs]
	pool      *pgxpool.Pool
	provider  embed.Provider
	resolver  embedResolver // opcional — resolve embed ao vivo (config global)
	defaultBS int
}

func NewEmbedBatchWorker(pool *pgxpool.Pool, p embed.Provider, resolver embedResolver, defaultBatchSize int) *EmbedBatchWorker {
	if defaultBatchSize <= 0 {
		defaultBatchSize = 100
	}
	return &EmbedBatchWorker{pool: pool, provider: p, resolver: resolver, defaultBS: defaultBatchSize}
}

// embedProvider devolve o embed provider corrente: resolver (config global) > .env.
func (w *EmbedBatchWorker) embedProvider(ctx context.Context) embed.Provider {
	if w.resolver != nil {
		if p := w.resolver.Embed(ctx); p != nil {
			return p
		}
	}
	return w.provider
}

// Timeout sobrescreve o JobTimeout default do River (60s). O Work drena em LOOP
// vários batches; em CPU (TEI bge-m3) cada batch leva ~30-46s, então 2+ batches
// estouram 60s → "context deadline exceeded" no meio do embed POST, job retry,
// chunks órfãos. 30min cobre haystacks grandes drenando em série. (Em GPU os
// batches são rápidos e isso vira folga.) Ver lesson_river_job_timeout_default.
func (w *EmbedBatchWorker) Timeout(*river.Job[EmbedBatchArgs]) time.Duration {
	return 30 * time.Minute
}

// RegisterEmbedWorker registra o EmbedBatchWorker no Workers do River.
// Caller (cmd/nexus/worker.go) só chama se provider de embed estiver configurado.
func RegisterEmbedWorker(workers *river.Workers, pool *pgxpool.Pool, p embed.Provider, resolver embedResolver, batchSize int) error {
	return river.AddWorkerSafely(workers, NewEmbedBatchWorker(pool, p, resolver, batchSize))
}

// maxBatchesPerJob é um teto de segurança pro loop de drenagem (evita um job
// eterno se algo ficar inserindo chunks sem parar). 1000 * batchSize cobre
// qualquer ingest real; se estourar, encadeia um sucessor.
const maxBatchesPerJob = 1000

func (w *EmbedBatchWorker) Work(ctx context.Context, job *river.Job[EmbedBatchArgs]) error {
	prov := w.embedProvider(ctx)
	if prov == nil {
		return errors.New("embed_batch: provider não configurado (verifique EMBED provider)")
	}

	batchSize := job.Args.BatchSize
	if batchSize <= 0 {
		batchSize = w.defaultBS
	}

	start := time.Now()

	// Drena em LOOP até esvaziar. Antes o worker processava 1 batch e encadeava
	// um sucessor via enqueueNextBatch — mas o UniqueOpts inclui JobStateRunning,
	// então o sucessor era deduplicado CONTRA O PRÓPRIO job (ainda running) e
	// pulado, deixando chunks órfãos pra qualquer ingest > batchSize. O loop
	// elimina a corrida (não depende mais do chain+dedup). drain_pending_chunks
	// usa SKIP LOCKED, então múltiplos workers não colidem.
	total := 0
	for iter := 0; ; iter++ {
		if iter >= maxBatchesPerJob {
			slog.Warn("embed_batch: teto de batches atingido, encadeando sucessor", "job_id", job.ID, "processed", total)
			return enqueueNextBatch(ctx, w.pool)
		}
		n, err := w.drainOneBatch(ctx, prov, batchSize)
		if err != nil {
			return err
		}
		total += n
		if n < batchSize {
			break // batch incompleto = não há mais chunks pendentes
		}
	}

	if total == 0 {
		slog.Info("embed_batch: no pending chunks", "job_id", job.ID)
		return nil
	}

	slog.Info("embed_batch done",
		"job_id", job.ID,
		"processed", total,
		"provider", prov.Name(),
		"model", prov.Model(),
		"latency_ms", time.Since(start).Milliseconds(),
	)
	return nil
}

// drainOneBatch processa até batchSize chunks pendentes (embedding NULL) e
// retorna quantos processou. 0 = nada pendente.
func (w *EmbedBatchWorker) drainOneBatch(ctx context.Context, prov embed.Provider, batchSize int) (int, error) {
	// Pega N chunks com embedding NULL via SECURITY DEFINER function.
	// drain_pending_chunks bypassa RLS internamente (owned por nexus_service);
	// worker pode rodar como nexus_app (sem BYPASSRLS direto). Day 14.
	rows, err := w.pool.Query(ctx, `SELECT id, content, organization_id FROM drain_pending_chunks($1)`, batchSize)
	if err != nil {
		return 0, fmt.Errorf("embed_batch: drain pending: %w", err)
	}

	type pending struct {
		ID    int64
		Text  string
		OrgID int64 // só pra logs/métricas
	}
	var items []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.ID, &p.Text, &p.OrgID); err != nil {
			rows.Close()
			return 0, fmt.Errorf("embed_batch: scan: %w", err)
		}
		items = append(items, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("embed_batch: rows err: %w", err)
	}

	if len(items) == 0 {
		return 0, nil
	}

	// Bate o provider de embed com o batch (o provider sub-divide se exceder o
	// limite do backend, ex. TEI --max-client-batch-size).
	texts := make([]string, len(items))
	for i, p := range items {
		texts[i] = p.Text
	}

	vecs, err := prov.Embed(ctx, texts, embed.InputTypeDocument)
	if err != nil {
		return 0, fmt.Errorf("embed_batch: provider call: %w", err)
	}
	if len(vecs) != len(items) {
		return 0, fmt.Errorf("embed_batch: provider returned %d vecs for %d chunks", len(vecs), len(items))
	}

	// UPDATE em batch via SECURITY DEFINER function (1 call por chunk).
	// Uma transação envelopa todos pra que falha parcial não deixe estado misto.
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("embed_batch: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for i, p := range items {
		if _, err := tx.Exec(ctx,
			"SELECT update_chunk_embedding($1, $2)",
			p.ID, pgvector.NewVector(vecs[i]),
		); err != nil {
			return 0, fmt.Errorf("embed_batch: update chunk %d: %w", p.ID, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("embed_batch: commit: %w", err)
	}

	return len(items), nil
}

// enqueueNextBatch agenda outro EmbedBatchJob (com dedup) pra continuar o drenamento.
// Dedup via UniqueOpts: se já tem job available/scheduled/running, Insert retorna
// o job existente com UniqueSkippedAsDuplicate=true (sem erro).
func enqueueNextBatch(ctx context.Context, pool *pgxpool.Pool) error {
	client, err := NewInsertOnlyClient(pool)
	if err != nil {
		return fmt.Errorf("embed_batch: chain client: %w", err)
	}
	if _, err := client.Insert(ctx, EmbedBatchArgs{}, &river.InsertOpts{}); err != nil {
		return fmt.Errorf("embed_batch: chain enqueue: %w", err)
	}
	return nil
}

// EnqueueEmbedBatch é o helper chamado pelo IngestWorker pra agendar
// processamento. Com pequeno delay pra agregar múltiplos ingests no batch.
// Dedup é automático via UniqueOpts em InsertOpts() do EmbedBatchArgs.
func EnqueueEmbedBatch(ctx context.Context, client *river.Client[pgx.Tx], delay time.Duration) error {
	if _, err := client.Insert(ctx, EmbedBatchArgs{}, &river.InsertOpts{
		ScheduledAt: time.Now().Add(delay),
	}); err != nil {
		return fmt.Errorf("enqueue embed batch: %w", err)
	}
	return nil
}
