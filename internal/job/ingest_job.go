package job

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
	"github.com/riverqueue/river"

	domains "github.com/nexusyn/engine/internal/core/domain"
	"github.com/nexusyn/engine/internal/core/ingest"
	"github.com/nexusyn/engine/internal/core/redact"
	"github.com/nexusyn/engine/internal/dateutil"
	"github.com/nexusyn/engine/internal/provider/embed"
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
	Domain         string         `json:"domain,omitempty"`  // memory | wiki | source | ...
	Project        string         `json:"project,omitempty"` // segmento de projeto na org; "" = global (NULL)
	Metadata       map[string]any `json:"metadata,omitempty"`
	// CreatedAt opcional: preserva a cronologia original no import (round-trip
	// export→import). nil = now() (comportamento padrão do ingest). Só o
	// POST /v1/import popula — o /v1/ingest público NÃO expõe (cliente comum
	// não backdata memória).
	CreatedAt *time.Time `json:"created_at,omitempty"`
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
	pool     *pgxpool.Pool
	resolver embedResolver // opcional — resolve embed ao vivo p/ dedup semântico; nil = só determinístico
}

func NewIngestWorker(pool *pgxpool.Pool, resolver embedResolver) *IngestWorker {
	return &IngestWorker{pool: pool, resolver: resolver}
}

// embedProvider resolve o embed provider ao vivo (config global), ou nil se o
// embed não estiver configurado (aí o dedup semântico é pulado e o ingest segue
// só com o dedup determinístico por content_hash).
func (w *IngestWorker) embedProvider(ctx context.Context) embed.Provider {
	if w.resolver != nil {
		return w.resolver.Embed(ctx)
	}
	return nil
}

// Limites anti denial-of-context (flooding): um doc gigante não pode inflar a base
// e deslocar contexto relevante. maxIngestRunes alinhado ao cap do MCP (~256KB);
// maxIngestChunks é o teto de chunks por doc.
const (
	maxIngestRunes  = 200_000
	maxIngestChunks = 400
	maxMetaBytes    = 16_384 // metadata JSONB acima disso é descartado (anti payload/flooding)
	// Dedup semântico (Módulo A): embedding page-level (title+content truncado) +
	// supersede de paráfrases acima do threshold de similaridade.
	maxDedupEmbedRunes = 8_000 // teto do texto embedado pro dedup (limite do provider)
	dedupMaxCosineDist = 0.08  // cosine distance máx p/ tratar como duplicata (similaridade >= 0.92)
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

	// Hash do conteúdo final (pós-redact/trunca) pro dedup determinístico (Módulo A).
	contentHash := hashContent(content)

	// Embedding page-level pro dedup semântico (Módulo A): pega paráfrases que o
	// content_hash não pega. Só roda se o embed provider estiver configurado; como o
	// ingest é async (worker), o embed síncrono aqui não pesa no add_memory.
	var pageVec []float32
	if prov := w.embedProvider(ctx); prov != nil {
		dedupText := args.Title + "\n" + content
		if r := []rune(dedupText); len(r) > maxDedupEmbedRunes {
			dedupText = string(r[:maxDedupEmbedRunes])
		}
		if vecs, eerr := prov.Embed(ctx, []string{dedupText}, embed.InputTypeDocument); eerr == nil && len(vecs) == 1 {
			pageVec = vecs[0]
		} else if eerr != nil {
			slog.Warn("ingest: embed p/ dedup semântico falhou (segue sem)", "err", eerr, "org_id", args.OrganizationID)
		}
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
	var deduped bool
	err = tenant.RunWithTenant(ctx, w.pool, args.OrganizationID, func(tx pgx.Tx) error {
		// 0. Dedup determinístico (Módulo A): re-insert literal? Se já existe page
		// VIGENTE com mesmo (org, domain, content_hash), é a MESMA memória → NOOP
		// (não duplica). Fecha a causa da colisão de slug — o insert era incondicional.
		var existingID int64
		derr := tx.QueryRow(ctx, `
			SELECT id FROM pages
			WHERE organization_id = $1 AND domain = $2 AND content_hash = $3 AND valid_to IS NULL
			LIMIT 1
		`, args.OrganizationID, domain, contentHash).Scan(&existingID)
		switch {
		case derr == nil:
			deduped = true
			slog.Info("ingest: dedup skip (content_hash igual)",
				"org_id", args.OrganizationID, "existing_page_id", existingID, "slug", slug)
			return nil
		case derr == pgx.ErrNoRows:
			// não é duplicata literal — segue pro INSERT
		default:
			return fmt.Errorf("dedup check: %w", derr)
		}

		// 0.5 Dedup semântico (Módulo A): supersede paráfrase muito similar e devolve
		// o arg de embedding (pgvector ou nil) pro INSERT da page nova.
		embArg, sdErr := semanticDedup(ctx, tx, args.OrganizationID, domain, contentHash, pageVec, slug)
		if sdErr != nil {
			return sdErr
		}

		// 1. INSERT page (sem embedding — chunks que carregam)
		err := tx.QueryRow(ctx, `
			INSERT INTO pages (organization_id, agent_id, slug, title, content, domain, project, source_type, metadata, content_hash, embedding, created_at)
			VALUES ($1, NULLIF($2, 0), $3, $4, $5, $6, NULLIF($7, ''), 'raw', $8::jsonb, $9, $10, COALESCE($11, now()))
			RETURNING id
		`,
			args.OrganizationID,
			args.AgentID,
			slug,
			args.Title,
			content,
			domain,
			args.Project, // NULLIF('') → NULL = global
			metaJSON,
			contentHash,
			embArg,
			args.CreatedAt, // nil → now() (só o import backdata)
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
	if deduped {
		return nil // NOOP: memória idêntica já existia — nada a embedar/extrair
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

// hashContent computa o SHA-256 do conteúdo normalizado (trim + lowercase +
// colapsa whitespace) — usado no dedup determinístico do ingest (Módulo A).
func hashContent(s string) []byte {
	norm := strings.ToLower(strings.TrimSpace(s))
	norm = strings.Join(strings.Fields(norm), " ")
	sum := sha256.Sum256([]byte(norm))
	return sum[:]
}

// semanticDedup faz o dedup semântico (Módulo A): se pageVec for muito similar
// (cosine >= 0.92) a uma page vigente do mesmo (org, domain), supersede a antiga
// (valid_to=now() + remove chunks do recall). Retorna o arg de embedding pro
// INSERT da page nova (pgvector.Vector) ou nil se não houve embed.
func semanticDedup(ctx context.Context, tx pgx.Tx, org int64, domain string, contentHash []byte, pageVec []float32, slug string) (any, error) {
	if pageVec == nil {
		return nil, nil
	}
	vec := pgvector.NewVector(pageVec)
	var simID int64
	var dist float64
	serr := tx.QueryRow(ctx, `
		SELECT id, (embedding <=> $4) AS dist FROM pages
		WHERE organization_id = $1 AND domain = $2 AND valid_to IS NULL
		  AND embedding IS NOT NULL AND content_hash IS DISTINCT FROM $3
		ORDER BY embedding <=> $4 LIMIT 1
	`, org, domain, contentHash, vec).Scan(&simID, &dist)
	switch {
	case serr == nil && dist <= dedupMaxCosineDist:
		if _, uerr := tx.Exec(ctx, `UPDATE pages SET valid_to = now() WHERE id = $1 AND valid_to IS NULL`, simID); uerr != nil {
			return vec, fmt.Errorf("dedup semantico supersede: %w", uerr)
		}
		if _, cerr := tx.Exec(ctx, `DELETE FROM chunks WHERE page_id = $1`, simID); cerr != nil {
			return vec, fmt.Errorf("dedup semantico cleanup: %w", cerr)
		}
		slog.Info("ingest: dedup semantico supersede (parafrase)",
			"org_id", org, "superseded_page_id", simID, "cosine", 1-dist, "slug", slug)
	case serr == nil, serr == pgx.ErrNoRows:
	default:
		return vec, fmt.Errorf("dedup semantico busca: %w", serr)
	}
	return vec, nil
}
