package search

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nexusyn/engine/internal/dateutil"
	"github.com/nexusyn/engine/internal/provider/embed"
	"github.com/nexusyn/engine/internal/tenant"
)

// embedResolver resolve, ao vivo, o embed provider da config global (interface
// fina pra não acoplar search ao modelresolver). Pode devolver nil.
type embedResolver interface {
	Embed(ctx context.Context) embed.Provider
}

// Service é o entrypoint de busca.
// Mantém deps (pool, embed provider) injetadas no construtor.
type Service struct {
	pool     *pgxpool.Pool
	embedder embed.Provider
	resolver embedResolver // opcional — resolve embed ao vivo (config global)
}

// NewService constrói o Service. embedder pode ser nil — modo FTS-only.
func NewService(pool *pgxpool.Pool, embedder embed.Provider) *Service {
	return &Service{pool: pool, embedder: embedder}
}

// EnableResolver liga a resolução ao vivo do embed (config global do operador).
// CRÍTICO: tem que ser o MESMO resolver do embed worker, senão query e chunks
// ficam em modelos diferentes e o retrieval quebra.
func (s *Service) EnableResolver(r embedResolver) { s.resolver = r }

// embedFor devolve o embed provider corrente: resolver (config global) > .env.
func (s *Service) embedFor(ctx context.Context) embed.Provider {
	if s.resolver != nil {
		if p := s.resolver.Embed(ctx); p != nil {
			return p
		}
	}
	return s.embedder
}

// Search executa busca híbrida (ou single-channel conforme Options.Mode).
//
// Fluxo hybrid:
//  1. Embeda a query via Jina (input_type=query)
//  2. Roda vector search (top-50 chunks) em paralelo com FTS pt-BR (top-50)
//  3. RRF fusion combina ambos → top-N final
//  4. Hidrata chunks com page metadata
//
// Tenancy: usa RunWithTenantReadOnly — RLS bloqueia rows de outro tenant.
func (s *Service) Search(ctx context.Context, orgID int64, opts Options) ([]Result, error) {
	opts.Defaults()
	if opts.Query == "" {
		return nil, fmt.Errorf("search: query vazia")
	}

	var results []Result
	err := tenant.RunWithTenantReadOnly(ctx, s.pool, orgID, func(tx pgx.Tx) error {
		switch opts.Mode {
		case ModeVector:
			ids, err := s.runVector(ctx, tx, opts)
			if err != nil {
				return err
			}
			scores := makeScores(ids)
			results, err = loadResults(ctx, tx, ids, scores, "vector", opts.AsOf)
			return err

		case ModeFTS:
			ids, err := ftsSearch(ctx, tx, opts.Query, opts.Limit, opts.Domain)
			if err != nil {
				return err
			}
			scores := makeScores(ids)
			results, err = loadResults(ctx, tx, ids, scores, "fts", opts.AsOf)
			return err

		case ModeHybrid:
			fallthrough
		default:
			return s.runHybrid(ctx, tx, opts, &results)
		}
	})
	return results, err
}

// runVector embeda a query e executa vector search.
func (s *Service) runVector(ctx context.Context, tx pgx.Tx, opts Options) ([]int64, error) {
	embedder := s.embedFor(ctx)
	if embedder == nil {
		return nil, fmt.Errorf("search: vector mode requer embed provider")
	}
	vecs, err := embedder.Embed(ctx, []string{opts.Query}, embed.InputTypeQuery)
	if err != nil {
		return nil, fmt.Errorf("search: embed query: %w", err)
	}
	if len(vecs) != 1 {
		return nil, fmt.Errorf("search: embed retornou %d vetores (esperado 1)", len(vecs))
	}
	return vectorSearch(ctx, tx, vecs[0], opts.Limit, opts.Domain)
}

// runHybrid roda vector + FTS + entity-match (Sprint 2.5), faz RRF fusion,
// aplica recency e date-anchor boosts, hidrata top-N.
// candidatePool = max(50, 5*limit) pra ter material suficiente pra fusion.
func (s *Service) runHybrid(ctx context.Context, tx pgx.Tx, opts Options, out *[]Result) error {
	candidatePool := opts.Limit * 5
	if candidatePool < 50 {
		candidatePool = 50
	}

	var vecIDs, ftsIDs, entIDs []int64
	var vecErr, ftsErr, entErr error

	// Vector (precisa embed)
	if embedder := s.embedFor(ctx); embedder != nil {
		vecs, err := embedder.Embed(ctx, []string{opts.Query}, embed.InputTypeQuery)
		if err != nil {
			vecErr = fmt.Errorf("embed query: %w", err)
		} else if len(vecs) == 1 {
			vecIDs, vecErr = vectorSearch(ctx, tx, vecs[0], candidatePool, opts.Domain)
		}
	}

	// FTS (sempre roda — não depende de provider externo)
	ftsIDs, ftsErr = ftsSearch(ctx, tx, opts.Query, candidatePool, opts.Domain)

	// Sprint 2.5 — entity-match (3º sinal). Trigram fuzzy em entities.name +
	// alias match exato. Pega chunks de pages onde entities mencionadas na
	// query foram extraídas. Falha graceful — sinal é opcional.
	// Sprint 3.2 — asOf passa snapshot temporal (entities/edges válidos no time).
	entIDs, entErr = entityMatchSearch(ctx, tx, opts.Query, candidatePool, opts.Domain, opts.AsOf)

	// Canal de data (migration 0022): se a query menciona datas, recupera os chunks
	// com o token de data canônico. Determinístico — pega o chunk da data exata que
	// vetor/FTS perdem por formato/idioma. No-op quando não há data na query.
	var dateIDs []int64
	var dateErr error
	if dq := dateutil.DateTSQuery(dateutil.StripContextDate(opts.Query)); dq != "" {
		dateIDs, dateErr = dateSearch(ctx, tx, dq, candidatePool, opts.Domain)
	}

	// Fase 1 GraphRAG — graph-expansion (5º canal, sob NEXUS_GRAPH_EXPANSION).
	// Expande 1-2 hops dos seeds da query e traz chunks das pages das vizinhas.
	// Off por padrão (modo observação); falha graceful como os demais canais.
	var graphIDs []int64
	var graphErr error
	if graphExpansionEnabled(ctx, tx) {
		graphIDs, graphErr = graphExpansionSearch(ctx, tx, opts.Query, candidatePool, opts.Domain, opts.AsOf)
	}

	// Se TODOS falharam, retorna erros wrapped
	if vecErr != nil && ftsErr != nil && entErr != nil {
		return fmt.Errorf("hybrid: todos canais falharam: vec=%w fts=%w ent=%w", vecErr, ftsErr, entErr)
	}

	// Canais com peso: vector/FTS em 1.0, entity-match com boost (EntityMatchWeight).
	// names é paralelo a lists/weights — usado só pela instrumentação (Fase 0 GraphRAG).
	lists := [][]int64{}
	weights := []float64{}
	names := []string{}
	if vecErr == nil && len(vecIDs) > 0 {
		lists = append(lists, vecIDs)
		weights = append(weights, 1.0)
		names = append(names, "vec")
	}
	if ftsErr == nil && len(ftsIDs) > 0 {
		lists = append(lists, ftsIDs)
		weights = append(weights, 1.0)
		names = append(names, "fts")
	}
	if entErr == nil && len(entIDs) > 0 {
		lists = append(lists, entIDs)
		weights = append(weights, EntityMatchWeight)
		names = append(names, "ent")
	}
	if dateErr == nil && len(dateIDs) > 0 {
		lists = append(lists, dateIDs)
		weights = append(weights, DateMatchWeight)
		names = append(names, "date")
	}
	if graphErr == nil && len(graphIDs) > 0 {
		lists = append(lists, graphIDs)
		weights = append(weights, GraphExpansionWeight)
		names = append(names, "graph")
	}

	// Fuse top candidatePool primeiro (não opts.Limit) pra deixar os boosts
	// re-ordenarem dentre os candidatos antes do trim final.
	fusedIDs := FuseRRFWeighted(lists, weights, candidatePool)
	scores := scoresFromRRFWeighted(lists, weights, fusedIDs)

	// Day 22 — recency boost: chunks recentes ganham peso adicional.
	// Sprint 1.7 — exempt preferences/lessons (sem decay), padrão OMEGA.
	if opts.RecencyHalfLifeDays > 0 && len(fusedIDs) > 0 {
		timestamps, err := loadChunkTimestamps(ctx, tx, fusedIDs)
		if err == nil {
			exempt, _ := loadDecayExemptChunks(ctx, tx, fusedIDs)
			scores = ApplyRecencyBoost(scores, timestamps, opts.RecencyHalfLifeDays, time.Now(), exempt, recencyExemptFloor)
		}
	}

	// Sprint 2.5 — date-anchored boost: detecta data na query, boosta chunks
	// com session_date próxima. Útil pra queries "what happened on March 15?".
	// Anchor zero (sem data detectada) → no-op.
	anchor := DetectDateAnchor(opts.Query, time.Now())
	if !anchor.IsZero() && len(fusedIDs) > 0 {
		sessionDates, sderr := loadChunkSessionDates(ctx, tx, fusedIDs)
		if sderr == nil {
			scores = ApplyDateAnchorBoost(scores, sessionDates, anchor, 1.0, 7.0)
		}
	}

	// Re-ordena por scores finais (com todos boosts aplicados)
	if opts.RecencyHalfLifeDays > 0 || !anchor.IsZero() {
		fusedIDs = ReorderByScore(fusedIDs, scores)
	}

	// Trim pro limit final. Diversify (count/aggregation): cap por-página pra
	// cobrir eventos esparsos em sessões raras, em vez de top-K puro lotado pela
	// conversa dominante. Senão, top-K por score.
	if opts.Diversify && len(fusedIDs) > opts.Limit {
		if pages, perr := loadChunkPages(ctx, tx, fusedIDs); perr == nil {
			fusedIDs = capPerPage(fusedIDs, pages, perPageCap, opts.Limit)
		} else if len(fusedIDs) > opts.Limit {
			fusedIDs = fusedIDs[:opts.Limit]
		}
	} else if len(fusedIDs) > opts.Limit {
		fusedIDs = fusedIDs[:opts.Limit]
	}

	// Fase 0 GraphRAG — instrumentação opcional (NEXUS_SEARCH_DEBUG): mede o valor
	// marginal de cada canal RRF sobre o resultado final. No-op por padrão.
	logChannelContribution(opts.Query, lists, names, fusedIDs)

	// Sprint 3.2 — passa asOf pra filtrar chunks por pages.created_at no hydrate.
	results, err := loadResults(ctx, tx, fusedIDs, scores, "hybrid", opts.AsOf)
	if err != nil {
		return err
	}
	*out = results
	return nil
}

// perPageCap é o máx de chunks por página na seleção diversificada (count
// queries). 3 espalha os slots por mais sessões sem perder densidade local.
const perPageCap = 3

// loadChunkPages mapeia chunk_id → page_id pros ids dados.
func loadChunkPages(ctx context.Context, tx pgx.Tx, ids []int64) (map[int64]int64, error) {
	out := make(map[int64]int64, len(ids))
	rows, err := tx.Query(ctx, `SELECT id, page_id FROM chunks WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, pid int64
		if err := rows.Scan(&cid, &pid); err != nil {
			return nil, err
		}
		out[cid] = pid
	}
	return out, rows.Err()
}

// capPerPage seleciona até `limit` ids preferindo diversidade de página: na 1ª
// passada pega ≤cap por página (na ordem ranqueada de ids); se faltar pro
// limit, faz backfill com o overflow (mantém a ordem). Garante `limit` itens
// mas espalha por mais páginas — surface de eventos esparsos em count queries.
func capPerPage(ids []int64, page map[int64]int64, cap, limit int) []int64 {
	perPage := make(map[int64]int, limit)
	primary := make([]int64, 0, limit)
	overflow := make([]int64, 0, len(ids))
	for _, id := range ids {
		p := page[id]
		if perPage[p] < cap {
			perPage[p]++
			primary = append(primary, id)
		} else {
			overflow = append(overflow, id)
		}
	}
	out := primary
	if len(out) < limit {
		out = append(out, overflow...)
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// makeScores atribui scores 1.0/rank pra single-channel (sem RRF).
func makeScores(ids []int64) map[int64]float64 {
	scores := make(map[int64]float64, len(ids))
	for i, id := range ids {
		scores[id] = 1.0 / float64(i+1)
	}
	return scores
}

// scoresFromRRFWeighted calcula o score RRF ponderado de cada id final (Result).
func scoresFromRRFWeighted(lists [][]int64, weights []float64, ids []int64) map[int64]float64 {
	out := make(map[int64]float64, len(ids))
	for _, id := range ids {
		out[id] = ScoreOfWeighted(lists, weights, id)
	}
	return out
}

// searchDebug liga a instrumentação de contribuição de canal (Fase 0 GraphRAG).
// Lido uma vez no load do pacote. NEXUS_SEARCH_DEBUG=1|true.
var searchDebug = os.Getenv("NEXUS_SEARCH_DEBUG") == "1" || os.Getenv("NEXUS_SEARCH_DEBUG") == "true"

// logChannelContribution loga, por canal RRF, quantos dos resultados finais o
// canal cobre (covers) e quantos são exclusivos dele — não aparecem em nenhum
// outro canal (excl). 'excl' alto = sinal insubstituível; 'excl' zero = canal
// redundante naquela query. Agregado ao longo de muitas queries, mede se o
// entity-match (e o futuro canal de graph-expansion) puxa peso próprio.
// No-op sem NEXUS_SEARCH_DEBUG.
func logChannelContribution(query string, lists [][]int64, names []string, finalIDs []int64) {
	if !searchDebug || len(finalIDs) == 0 {
		return
	}
	final := make(map[int64]struct{}, len(finalIDs))
	for _, id := range finalIDs {
		final[id] = struct{}{}
	}
	// presença, por canal, de cada id que chegou ao resultado final
	inChannel := make([]map[int64]struct{}, len(lists))
	for i, l := range lists {
		m := make(map[int64]struct{})
		for _, id := range l {
			if _, ok := final[id]; ok {
				m[id] = struct{}{}
			}
		}
		inChannel[i] = m
	}
	attrs := []any{"query", query, "final", len(finalIDs)}
	for i := range lists {
		exclusive := 0
		for id := range inChannel[i] {
			only := true
			for j := range lists {
				if j == i {
					continue
				}
				if _, ok := inChannel[j][id]; ok {
					only = false
					break
				}
			}
			if only {
				exclusive++
			}
		}
		attrs = append(attrs, names[i]+"_covers", len(inChannel[i]), names[i]+"_excl", exclusive)
	}
	slog.Info("search: channel contribution", attrs...)
}
