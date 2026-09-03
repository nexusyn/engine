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
	"github.com/nexusyn/engine/internal/provider/rerank"
	"github.com/nexusyn/engine/internal/tenant"
)

// embedResolver resolve, ao vivo, o embed provider da config global (interface
// fina pra não acoplar search ao modelresolver). Pode devolver nil.
type embedResolver interface {
	Embed(ctx context.Context) embed.Provider
}

// rerankResolver resolve, ao vivo, o reranker da config global (Módulo B — usado
// só pra reranquear o canal do grafo). Pode devolver nil.
type rerankResolver interface {
	Reranker(ctx context.Context) rerank.Provider
}

// Service é o entrypoint de busca.
// Mantém deps (pool, embed provider) injetadas no construtor.
type Service struct {
	pool      *pgxpool.Pool
	embedder  embed.Provider
	resolver  embedResolver  // opcional — resolve embed ao vivo (config global)
	rerankRes rerankResolver // opcional — resolve reranker ao vivo (Módulo B, grafo)
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

// EnableReranker liga a resolução ao vivo do reranker (Módulo B). Usado SÓ pra
// reranquear o canal do grafo (sob NEXUS_GRAPH_RERANK); não afeta os demais
// canais nem o rerank final do query.Service. Aditivo.
func (s *Service) EnableReranker(r rerankResolver) { s.rerankRes = r }

// rerankerFor devolve o reranker corrente (config global ao vivo), ou nil.
func (s *Service) rerankerFor(ctx context.Context) rerank.Provider {
	if s.rerankRes != nil {
		return s.rerankRes.Reranker(ctx)
	}
	return nil
}

// Search executa busca híbrida (ou single-channel conforme Options.Mode).
//
// Fluxo hybrid (ver runHybrid):
//  1. Embeda a query via Jina (input_type=query)
//  2. Roda vector search, FTS pt-BR, entity-match, canal de data e graph-expansion
//     EM SÉRIE — todos os canais compartilham a MESMA pgx.Tx (aberta uma vez por
//     Search, via RunWithTenantReadOnly) e pgx.Tx não é seguro pra uso concorrente
//     por múltiplas goroutines. NÃO roda em paralelo hoje, apesar do nome do passo
//     "hybrid" sugerir — latência do hybrid = soma da latência de cada canal.
//     Paralelizar exigiria uma conexão/transação read-only PRÓPRIA por canal (via
//     errgroup) e tocaria as implementações dos canais em queries.go/entity_match.go/
//     date_anchor.go/graph_expansion.go — avaliado e ADIADO (2026-07-02): mudança
//     de ranking exige validação em bench antes de prod, e o refactor aumentaria o
//     nº de conexões simultâneas por query bem quando o pool está sendo ajustado
//     (DB_MAX_CONNS). Ver NEX-005 no registro de auditoria.
//  3. RRF fusion combina os canais → top-N final
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
			ids, err := ftsSearch(ctx, tx, opts.Query, opts.Limit, opts.Domain, opts.Project)
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
	return vectorSearch(ctx, tx, vecs[0], opts.Limit, opts.Domain, opts.Project)
}

// runHybrid roda vector + FTS + entity-match (Sprint 2.5) + data + graph-expansion
// EM SÉRIE (todos na MESMA tx recebida — pgx.Tx não é concorrency-safe), faz RRF
// fusion, aplica recency e date-anchor boosts, hidrata top-N. candidatePool =
// max(50, 5*limit) pra ter material suficiente pra fusion.
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
			vecIDs, vecErr = vectorSearch(ctx, tx, vecs[0], candidatePool, opts.Domain, opts.Project)
		}
	}

	// FTS (sempre roda — não depende de provider externo)
	ftsIDs, ftsErr = ftsSearch(ctx, tx, opts.Query, candidatePool, opts.Domain, opts.Project)

	// Sprint 2.5 — entity-match (3º sinal). Trigram fuzzy em entities.name +
	// alias match exato. Pega chunks de pages onde entities mencionadas na
	// query foram extraídas. Falha graceful — sinal é opcional.
	// Sprint 3.2 — asOf passa snapshot temporal (entities/edges válidos no time).
	entIDs, entErr = entityMatchSearch(ctx, tx, opts.Query, candidatePool, opts.Domain, opts.Project, opts.AsOf)

	// Canal de data (migration 0022): se a query menciona datas, recupera os chunks
	// com o token de data canônico. Determinístico — pega o chunk da data exata que
	// vetor/FTS perdem por formato/idioma. No-op quando não há data na query.
	var dateIDs []int64
	var dateErr error
	if dq := dateutil.DateTSQuery(dateutil.StripContextDate(opts.Query)); dq != "" {
		dateIDs, dateErr = dateSearch(ctx, tx, dq, candidatePool, opts.Domain, opts.Project)
	}

	// Fase 1 GraphRAG — graph-expansion (5º canal, sob NEXUS_GRAPH_EXPANSION).
	// Expande 1-2 hops dos seeds da query e traz chunks das pages das vizinhas.
	// Off por padrão (modo observação); falha graceful como os demais canais.
	var graphIDs []int64
	var graphErr error
	if graphExpansionEnabled(ctx, tx) {
		graphIDs, graphErr = graphExpansionSearch(ctx, tx, opts.Query, candidatePool, opts.Domain, opts.Project, opts.AsOf)
	}

	// Módulo B — rerank do canal do grafo (cross-encoder, PRÉ-FUSÃO). ADITIVO e gated
	// (NEXUS_GRAPH_RERANK, default OFF): reordena graphIDs por relevância real à query
	// ANTES do RRF — não toca os demais canais. Reusa o reranker configurado. Graceful.
	if len(graphIDs) > 1 && graphRerankEnabled() {
		if rk := s.rerankerFor(ctx); rk != nil {
			graphIDs = s.rerankGraphChannel(ctx, tx, opts.Query, graphIDs, rk)
		}
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
			// maxBoost=4.0 (não 1.0): em queries temporais a resposta é
			// semanticamente DESCONECTADA da pergunta ("o que fiz dia X?" vs a
			// resposta) — só a data conecta. Com ×2 (maxBoost=1) o chunk certo,
			// fraco no RRF, não subia ao top-K; ×5 na data exata garante que a
			// sessão da data domine. Falloff 7d mantido (cobre "mês passado").
			scores = ApplyDateAnchorBoost(scores, sessionDates, anchor, 4.0, 7.0)
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

// loadChunkContent carrega o texto dos chunks por id (Módulo B — rerank do canal
// do grafo). RLS aplica via tx do tenant; ids ausentes ficam fora do map.
func loadChunkContent(ctx context.Context, tx pgx.Tx, ids []int64) (map[int64]string, error) {
	out := make(map[int64]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := tx.Query(ctx, `SELECT id, content FROM chunks WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var content string
		if err := rows.Scan(&id, &content); err != nil {
			return nil, err
		}
		out[id] = content
	}
	return out, rows.Err()
}

// rerankGraphChannel reordena os chunks do canal do grafo pela relevância semântica
// à query (cross-encoder), pra o canal entrar LIMPO no RRF — relevância real, não só
// hop-decay. Reusa o reranker já configurado (jina-reranker-v3). Falha graceful:
// qualquer erro/vazio → ordem original (nunca degrada o canal).
func (s *Service) rerankGraphChannel(ctx context.Context, tx pgx.Tx, query string, ids []int64, reranker rerank.Provider) []int64 {
	contents, err := loadChunkContent(ctx, tx, ids)
	if err != nil || len(contents) == 0 {
		return ids
	}
	// docs paralelo a kept, na ordem original dos ids (só os com content).
	docs := make([]string, 0, len(ids))
	kept := make([]int64, 0, len(ids))
	for _, id := range ids {
		if c := contents[id]; c != "" {
			docs = append(docs, c)
			kept = append(kept, id)
		}
	}
	if len(docs) < 2 {
		return ids
	}
	ranked, err := reranker.Rerank(ctx, query, docs, len(docs))
	if err != nil || len(ranked) == 0 {
		return ids
	}
	if out := reorderByRanked(kept, ranked); len(out) > 0 {
		return out
	}
	return ids
}

// reorderByRanked mapeia o resultado do reranker (item.Index na lista docs) de volta
// pros chunk ids, na ordem ranqueada. Índices fora do range são ignorados (defensivo).
func reorderByRanked(kept []int64, ranked []rerank.Result) []int64 {
	out := make([]int64, 0, len(ranked))
	for _, item := range ranked {
		if item.Index >= 0 && item.Index < len(kept) {
			out = append(out, kept[item.Index])
		}
	}
	return out
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
