package job

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/nexusyn/engine/internal/core/compile"
	"github.com/nexusyn/engine/internal/core/ingest"
	"github.com/nexusyn/engine/internal/dateutil"
	"github.com/nexusyn/engine/internal/provider/llm"
	"github.com/nexusyn/engine/internal/tenant"
)

// compileBatchSize — fontes processadas por chamada de LLM. Pequeno de propósito:
// JSON de saída grande trunca (limite de tokens) → parse falha. Lotes pequenos
// mantêm cada saída dentro do MaxTokens.
const compileBatchSize = 3

// compileDedupThreshold — similaridade de conteúdo (pg_trgm, 0..1) acima da qual
// duas páginas derivadas são consideradas o MESMO fato (atualiza em vez de
// duplicar). 0.6 pega a deriva de slug sem fundir decisões genuinamente distintas.
const compileDedupThreshold = 0.6

// RunCompile destila `sources` (memory/knowledge) nas lentes `targets`
// (wiki/lesson/decision/error), por lotes pequenos com PACING + RETRY, persiste
// por UPSERT e liga ao grafo (extract entities + re-embed). É o núcleo do compile,
// compartilhado pelo handler HTTP (CompileHandler) e pelo job periódico
// (CompileOrgWorker). Falha de lote é coletada, não derruba o resto. Idempotente.
//
// Pacing/retry existem porque o MiniMax degrada/throttle após ~13 chamadas rápidas
// seguidas: 3s entre lotes + 3 tentativas com backoff (30s, 60s), timeout 240s/chamada.
func RunCompile(ctx context.Context, pool *pgxpool.Pool, insertClient *river.Client[pgx.Tx], provider llm.Provider, orgID, agentID int64, sources []compile.Doc, targets []string) (map[string]int, []string) {
	counts := map[string]int{}
	var failures []string

	// Compila POR PROJETO: agrupa as fontes pelo seu project e roda o pipeline
	// separado por grupo. Assim cada página derivada herda o project das suas
	// memórias-fonte (proveniência) — memórias globais (project "") geram derivados
	// globais. Sem isto o derivado nascia sempre global (project NULL) e vazava em
	// qualquer filtro `project=X` (que traz "projeto X + globais").
	for _, grp := range groupByProject(sources) {
		for _, tgtName := range targets {
			tgt, ok := compile.Targets[tgtName]
			if !ok {
				continue
			}
			for start := 0; start < len(grp.docs); start += compileBatchSize {
				end := start + compileBatchSize
				if end > len(grp.docs) {
					end = len(grp.docs)
				}
				batch := grp.docs[start:end]

				existing, _ := gatherDomainDocs(ctx, pool, orgID, tgt.Domain, 20)

				// Pacing: respira entre lotes — dá fôlego pro MiniMax acompanhar.
				time.Sleep(3 * time.Second)

				// Retry com backoff (30s, 60s): throttle é transitório.
				var res compile.Result
				var cerr error
				for attempt := 0; attempt < 3; attempt++ {
					if attempt > 0 {
						time.Sleep(time.Duration(attempt) * 30 * time.Second)
					}
					cctx, c := context.WithTimeout(ctx, 240*time.Second)
					res, cerr = compile.Synthesize(cctx, provider, tgt.System, batch, existing)
					c()
					if cerr == nil {
						break
					}
				}
				if cerr != nil {
					failures = append(failures, fmt.Sprintf("%s/%s/lote%d: %v", labelProject(grp.project), tgtName, start/compileBatchSize, cerr))
					continue
				}
				// Proveniência: liga as páginas compiladas às memórias-fonte do lote.
				batchIDs := make([]int64, 0, len(batch))
				for _, d := range batch {
					if d.ID > 0 {
						batchIDs = append(batchIDs, d.ID)
					}
				}
				saved, perr := persistPages(ctx, pool, insertClient, orgID, agentID, tgt.Domain, grp.project, res.Pages, batchIDs)
				if perr != nil {
					failures = append(failures, fmt.Sprintf("%s/%s/lote%d persist: %v", labelProject(grp.project), tgtName, start/compileBatchSize, perr))
					continue
				}
				counts[tgt.Domain] += saved
			}
		}
	}

	// Re-embed das páginas novas/atualizadas.
	if insertClient != nil {
		_ = EnqueueEmbedBatch(ctx, insertClient, 1*time.Second)
	}
	return counts, failures
}

// projectGroup são as fontes de um mesmo project, compiladas juntas.
type projectGroup struct {
	project string
	docs    []compile.Doc
}

// groupByProject particiona as fontes por project preservando a ordem de primeira
// aparição de cada project (determinístico — sem depender da ordem de iteração de
// map). Fontes com project "" formam o grupo global. É a peça que dá a cada
// derivado o project das suas memórias-fonte (proveniência).
func groupByProject(sources []compile.Doc) []projectGroup {
	idx := map[string]int{}
	var groups []projectGroup
	for _, d := range sources {
		i, ok := idx[d.Project]
		if !ok {
			i = len(groups)
			idx[d.Project] = i
			groups = append(groups, projectGroup{project: d.Project})
		}
		groups[i].docs = append(groups[i].docs, d)
	}
	return groups
}

// labelProject rotula o grupo global nas mensagens de falha.
func labelProject(p string) string {
	if p == "" {
		return "(global)"
	}
	return p
}

// gatherDomainDocs lê até `limit` páginas vivas de um domínio (pra integração).
func gatherDomainDocs(ctx context.Context, pool *pgxpool.Pool, orgID int64, domain string, limit int) ([]compile.Doc, error) {
	var out []compile.Doc
	err := tenant.RunWithTenantReadOnly(ctx, pool, orgID, func(tx pgx.Tx) error {
		rows, e := tx.Query(ctx,
			`SELECT title, content FROM pages
			 WHERE organization_id = $1 AND valid_to IS NULL AND domain = $2
			 ORDER BY created_at DESC LIMIT $3`, orgID, domain, limit)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var t, c string
			if e := rows.Scan(&t, &c); e != nil {
				return e
			}
			out = append(out, compile.Doc{Title: t, Content: c})
		}
		return rows.Err()
	})
	return out, err
}

// persistPages faz UPSERT por (slug, project) das páginas no domínio + re-chunk.
// Tx própria (lote isolado). Retorna quantas páginas foram gravadas. Enfileira
// extract de entidades pra ligar as páginas geradas ao grafo.
//
// `project` é o segmento das memórias-fonte deste lote ("" = global): grava no
// derivado e ESCOPA o dedup (UPSERT-por-slug e near-dup de conteúdo) por project,
// pra que projetos distintos com o mesmo slug/conteúdo NÃO colidam nem sobrescrevam
// um ao outro.
func persistPages(ctx context.Context, pool *pgxpool.Pool, insertClient *river.Client[pgx.Tx], orgID, agentID int64, domain, project string, pages []compile.Page, sourceIDs []int64) (int, error) {
	if len(pages) == 0 {
		return 0, nil
	}
	// Proveniência (auditoria): registra que a página é compilada e de quais memórias-fonte.
	meta, _ := json.Marshal(map[string]any{"compiled": true, "source_page_ids": sourceIDs})
	saved := 0
	seen := map[string]bool{}
	var pageIDs []int64
	err := tenant.RunWithTenant(ctx, pool, orgID, func(tx pgx.Tx) error {
		for _, p := range pages {
			slug := p.Slug
			if slug == "" {
				slug = ingest.Slug(p.Title)
			}
			if slug == "" || seen[slug] {
				continue
			}
			seen[slug] = true
			title := p.Title
			if title == "" {
				title = slug
			}

			// Dedup em 2 níveis pra sobreviver à DERIVA DE SLUG (o LLM gera slug
			// levemente diferente pro mesmo fato entre rodadas → UPSERT-por-slug
			// sozinho duplicava):
			//   1) slug exato (fast-path, slug estável);
			//   2) senão, similaridade de CONTEÚDO (pg_trgm ≥ compileDedupThreshold)
			//      no mesmo domínio → atualiza a página existente em vez de inserir.
			//   3) senão, é novo de verdade → insere.
			var pageID int64
			ct, e := tx.Exec(ctx,
				`UPDATE pages SET title = $3, content = $4, source_type = 'compiled', agent_id = NULLIF($6, 0), metadata = $7::jsonb
				 WHERE organization_id = $1 AND slug = $2 AND domain = $5 AND project IS NOT DISTINCT FROM NULLIF($8, '') AND valid_to IS NULL`,
				orgID, slug, title, p.Content, domain, agentID, meta, project)
			if e != nil {
				return e
			}
			if ct.RowsAffected() > 0 {
				if e := tx.QueryRow(ctx,
					`SELECT id FROM pages WHERE organization_id = $1 AND slug = $2 AND domain = $3 AND project IS NOT DISTINCT FROM NULLIF($4, '') AND valid_to IS NULL`,
					orgID, slug, domain, project).Scan(&pageID); e != nil {
					return e
				}
				if _, e := tx.Exec(ctx, `DELETE FROM chunks WHERE page_id = $1 AND organization_id = $2`, pageID, orgID); e != nil {
					return e
				}
			} else {
				var dupID int64
				derr := tx.QueryRow(ctx,
					`SELECT id FROM pages
					 WHERE organization_id = $1 AND domain = $2 AND project IS NOT DISTINCT FROM NULLIF($5, '') AND valid_to IS NULL
					   AND similarity(left(content, 500), left($3, 500)) >= $4
					 ORDER BY similarity(left(content, 500), left($3, 500)) DESC LIMIT 1`,
					orgID, domain, p.Content, compileDedupThreshold, project).Scan(&dupID)
				switch {
				case derr == nil:
					// near-dup de outra rodada (slug derivou) → atualiza ela.
					if _, e := tx.Exec(ctx,
						`UPDATE pages SET title = $2, content = $3, source_type = 'compiled', agent_id = NULLIF($4, 0), metadata = $5::jsonb
						 WHERE id = $1`, dupID, title, p.Content, agentID, meta); e != nil {
						return e
					}
					if _, e := tx.Exec(ctx, `DELETE FROM chunks WHERE page_id = $1 AND organization_id = $2`, dupID, orgID); e != nil {
						return e
					}
					pageID = dupID
				case errors.Is(derr, pgx.ErrNoRows):
					if e := tx.QueryRow(ctx,
						`INSERT INTO pages (organization_id, agent_id, slug, title, content, domain, project, source_type, metadata)
						 VALUES ($1, NULLIF($2, 0), $3, $4, $5, $6, NULLIF($7, ''), 'compiled', $8::jsonb) RETURNING id`,
						orgID, agentID, slug, title, p.Content, domain, project, meta).Scan(&pageID); e != nil {
						return e
					}
				default:
					return derr
				}
			}
			for _, c := range ingest.ChunkText(p.Content, ingest.DefaultChunkConfig()) {
				if _, e := tx.Exec(ctx,
					`INSERT INTO chunks (organization_id, page_id, position, content, dates) VALUES ($1, $2, $3, $4, $5)`,
					orgID, pageID, c.Position, c.Content, dateutil.DateSearchTokens(c.Content)); e != nil {
					return e
				}
			}
			pageIDs = append(pageIDs, pageID)
			saved++
		}
		return nil
	})
	if err == nil && insertClient != nil {
		for _, pid := range pageIDs {
			_ = EnqueueExtractEntities(ctx, insertClient, orgID, pid)
		}
	}
	return saved, err
}
