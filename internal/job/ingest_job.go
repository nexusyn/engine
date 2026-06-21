package job

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	domains "github.com/nexusyn/engine/internal/core/domain"
	"github.com/nexusyn/engine/internal/core/ingest"
	"github.com/nexusyn/engine/internal/core/redact"
	"github.com/nexusyn/engine/internal/dateutil"
	"github.com/nexusyn/engine/internal/tenant"
)

// IngestArgs são os argumentos do IngestJob.
//
// IMPORTANTE: OrganizationID é OBRIGATÓRIO. Workers River rodam fora do ciclo
// HTTP — o middleware de auth não está aqui. O job DEVE carregar o tenant_id
// nos args pra que o worker possa fazer RunWithTenant(orgID, fn).
type IngestArgs struct {
	OrganizationID int64          `json:"organization_id"`
	AgentID        int64          `json:"agent_id,omitempty"`
	Title          string         `json:"title"`
	Content        string         `json:"content"`
	Domain         string         `json:"domain,omitempty"` // memory | wiki | source | ...
	Metadata       map[string]any `json:"metadata,omitempty"`
}

func (IngestArgs) Kind() string { return "ingest" }

func (IngestArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       "ingest",
		MaxAttempts: 3,
	}
}

// IngestWorker processa IngestJobs:
//  1. Gera slug do title
//  2. Insere page (sem embedding — embedding fica nos chunks)
//  3. Chunka content (sliding window com overlap)
//  4. Insere N chunks (embedding NULL — async)
//  5. Enfileira EmbedBatchJob (dedup) pra calcular embeddings
type IngestWorker struct {
	river.WorkerDefaults[IngestArgs]
	pool *pgxpool.Pool
}

func NewIngestWorker(pool *pgxpool.Pool) *IngestWorker {
	return &IngestWorker{pool: pool}
}

// Limites anti denial-of-context (flooding): um doc gigante não pode inflar a base
// e deslocar contexto relevante. maxIngestRunes alinhado ao cap do MCP (~256KB);
// maxIngestChunks é o teto de chunks por doc.
const (
	maxIngestRunes  = 200_000
	maxIngestChunks = 400
	maxMetaBytes    = 16_384 // metadata JSONB acima disso é descartado (anti payload/flooding)
)

func (w *IngestWorker) Work(ctx context.Context, job *river.Job[IngestArgs]) error {
	args := job.Args

	if args.OrganizationID <= 0 {
		return fmt.Errorf("IngestJob: OrganizationID inválido: %d", args.OrganizationID)
	}
	if args.Content == "" {
		return fmt.Errorf("IngestJob: Content vazio")
	}

	// Whitelist de domínio: agente só escreve memory/knowledge/guideline/skill; derivados
	// (wiki/lesson/decision/error) viram "memory" (anti forja de autoridade). O compile
	// persiste os derivados por outro caminho, então isto não afeta a destilação.
	domain := domains.NormalizeInput(args.Domain)

	// Mascara segredos de alta confiança ANTES de persistir/chunkar — credenciais não
	// viram chunk recuperável em texto claro (anti-exfiltração via retrieval).
	content, redacted := redact.Redact(args.Content)
	if redacted > 0 {
		slog.Warn("ingest: segredos mascarados no conteúdo", "count", redacted, "org_id", args.OrganizationID)
	}

	// Limite de tamanho (anti-flooding): trunca em runa (UTF-8 safe).
	if r := []rune(content); len(r) > maxIngestRunes {
		slog.Warn("ingest: conteúdo truncado pelo limite", "from_runes", len(r), "to_runes", maxIngestRunes, "org_id", args.OrganizationID)
		content = string(r[:maxIngestRunes])
	}

	slug := ingest.Slug(args.Title)
	chunks := ingest.ChunkText(content, ingest.DefaultChunkConfig())
	if len(chunks) > maxIngestChunks {
		slog.Warn("ingest: chunks truncados pelo limite", "from", len(chunks), "to", maxIngestChunks, "org_id", args.OrganizationID)
		chunks = chunks[:maxIngestChunks]
	}

	slog.Info("ingest start",
		"job_id", job.ID,
		"org_id", args.OrganizationID,
		"agent_id", args.AgentID,
		"domain", domain,
		"title", args.Title,
		"slug", slug,
		"content_len", len(content),
		"n_chunks", len(chunks),
	)

	// pgx não tem encode plan default pra map[string]any → jsonb. Serializamos
	// manualmente pra []byte (jsonb aceita) — fix do bug pego no bench Day 21.
	metaJSON, err := json.Marshal(args.Metadata)
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}
	if len(args.Metadata) == 0 {
		metaJSON = []byte(`{}`)
	}
	// Cap de metadata: JSONB arbitrário/gigante é descartado (anti payload/flooding via metadata).
	if len(metaJSON) > maxMetaBytes {
		slog.Warn("ingest: metadata descartada pelo limite", "bytes", len(metaJSON), "org_id", args.OrganizationID)
		metaJSON = []byte(`{}`)
	}

	var pageID int64
	err = tenant.RunWithTenant(ctx, w.pool, args.OrganizationID, func(tx pgx.Tx) error {
		// 1. INSERT page (sem embedding — chunks que carregam)
		err := tx.QueryRow(ctx, `
			INSERT INTO pages (organization_id, agent_id, slug, title, content, domain, source_type, metadata)
			VALUES ($1, NULLIF($2, 0), $3, $4, $5, $6, 'raw', $7::jsonb)
			RETURNING id
		`,
			args.OrganizationID,
			args.AgentID,
			slug,
			args.Title,
			content,
			domain,
			metaJSON,
		).Scan(&pageID)
		if err != nil {
			return fmt.Errorf("insert page: %w", err)
		}

		// 2. INSERT chunks (em batch via COPY ou loop — loop é fine pra começo)
		for _, c := range chunks {
			if _, err := tx.Exec(ctx, `
				INSERT INTO chunks (organization_id, page_id, position, content, dates)
				VALUES ($1, $2, $3, $4, $5)
			`, args.OrganizationID, pageID, c.Position, c.Content, dateutil.DateSearchTokens(c.Content)); err != nil {
				return fmt.Errorf("insert chunk %d: %w", c.Position, err)
			}
		}

		slog.Info("ingest done",
			"job_id", job.ID,
			"page_id", pageID,
			"chunks_inserted", len(chunks),
		)
		return nil
	})
	if err != nil {
		return err
	}

	// Trigger follow-up jobs async em goroutine detached (sobrevive ao return do worker).
	// Ignora ctx do River pra que enqueue rode mesmo se job timeout estourar.
	// EmbedBatch e ExtractEntities rodam em PARALELO — não há dependência (LLM
	// lê content, não embedding).
	//nolint:contextcheck // detach intencional (enqueue de follow-up job)
	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		client, cerr := NewInsertOnlyClient(w.pool)
		if cerr != nil {
			slog.Warn("ingest: client for follow-up jobs", "err", cerr)
			return
		}
		if eerr := EnqueueEmbedBatch(bgCtx, client, 2*time.Second); eerr != nil {
			slog.Warn("ingest: enqueue embed batch", "err", eerr)
		}
		if eerr := EnqueueExtractEntities(bgCtx, client, args.OrganizationID, pageID); eerr != nil {
			slog.Warn("ingest: enqueue extract entities", "err", eerr)
		}
	}()

	return nil
}
