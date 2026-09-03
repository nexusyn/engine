package search

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pgvector/pgvector-go"
)

// maxVectorDistance é o gate de threshold semântico (cleanroom, item E).
// Cosine distance (<=>) varia 0 (idêntico) → 1 (ortogonal) → 2 (oposto).
// Candidatos acima desse teto são ruído near-ortogonal e nunca deveriam entrar
// na fusão — cortá-los antes do RRF é barato e evita que rank-based promova lixo
// quando a query tem poucos vizinhos reais. Teto FROUXO de propósito: só remove
// a cauda claramente irrelevante; conteúdo relacionado fica bem abaixo de 1.0
// (lição rerank-candidates: jamais derrubar o chunk-resposta por gate agressivo).
const maxVectorDistance = 1.0

// vectorSearch retorna chunks ordenados por similaridade semântica (cosine).
// Opera dentro da transação passada — RLS já está bindada via RunWithTenant.
func vectorSearch(ctx context.Context, tx pgx.Tx, queryVec []float32, limit int, domain, project string) ([]int64, error) {
	if len(queryVec) == 0 {
		return nil, fmt.Errorf("search: query embedding vazio")
	}
	if limit <= 0 {
		limit = 50
	}

	q := `
		SELECT c.id
		FROM chunks c
		JOIN pages p ON p.id = c.page_id
		WHERE c.embedding IS NOT NULL`
	args := []any{pgvector.NewVector(queryVec)}
	if domain != "" {
		args = append(args, domain)
		q += ` AND p.domain = ` + intToParam(len(args))
	}
	if project != "" {
		// Memória por projeto: traz o projeto + as globais (project IS NULL). F0.
		args = append(args, project)
		q += ` AND (p.project = ` + intToParam(len(args)) + ` OR p.project IS NULL)`
	}
	// Gate de threshold semântico: descarta candidatos near-ortogonais (item E).
	q += fmt.Sprintf(` AND (c.embedding <=> $1) <= %g`, maxVectorDistance)
	q += ` ORDER BY c.embedding <=> $1 LIMIT ` + intToParam(len(args)+1)
	args = append(args, limit)

	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("vector search: %w", err)
	}
	defer rows.Close()

	out := make([]int64, 0, limit)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("vector scan: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ftsSearch retorna chunks ordenados por ts_rank.
//
// IDIOMA: a config de FTS (ftsConfig) precisa bater com a usada nas colunas
// geradas content_tsv (ver migration 0021). Default 'english' — o produto e o
// benchmark (LongMemEval) são em inglês; stemming inglês normaliza conjugação
// ("attend"/"attending"). TODO futuro: idioma por-org (coluna lang + tsvector
// por trigger) pra atender corpora PT sem degradar.
func ftsSearch(ctx context.Context, tx pgx.Tx, query string, limit int, domain, project string) ([]int64, error) {
	if query == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 50
	}

	// ftsConfig é literal compile-time (não input) — seguro interpolar no SQL.
	const ftsConfig = "english"
	q := `
		SELECT c.id
		FROM chunks c
		JOIN pages p ON p.id = c.page_id
		WHERE c.content_tsv @@ plainto_tsquery('` + ftsConfig + `', $1)`
	args := []any{query}
	if domain != "" {
		args = append(args, domain)
		q += ` AND p.domain = ` + intToParam(len(args))
	}
	if project != "" {
		// Memória por projeto: traz o projeto + as globais (project IS NULL). F0.
		args = append(args, project)
		q += ` AND (p.project = ` + intToParam(len(args)) + ` OR p.project IS NULL)`
	}
	q += ` ORDER BY ts_rank(c.content_tsv, plainto_tsquery('` + ftsConfig + `', $1)) DESC LIMIT ` + intToParam(len(args)+1)
	args = append(args, limit)

	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("fts search: %w", err)
	}
	defer rows.Close()

	out := make([]int64, 0, limit)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("fts scan: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// loadResults hydrata chunk IDs em Result completo (com title, slug, etc.).
// Sprint 3.2: aceita asOf opcional — filtra chunks cuja page foi criada após
// esse timestamp (time-travel). Quando nil, retorna todos.
func loadResults(ctx context.Context, tx pgx.Tx, ids []int64, scores map[int64]float64, source string, asOf *time.Time) ([]Result, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	sql := `
		SELECT c.id, c.page_id, c.position, c.content, p.title, p.slug, p.domain, COALESCE(p.project, '')
		FROM chunks c
		JOIN pages p ON p.id = c.page_id
		WHERE c.id = ANY($1)`
	args := []any{ids}
	if asOf != nil {
		sql += ` AND p.created_at <= $2`
		args = append(args, *asOf)
	}

	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("load results: %w", err)
	}
	defer rows.Close()

	byID := make(map[int64]Result, len(ids))
	for rows.Next() {
		var r Result
		if err := rows.Scan(&r.ChunkID, &r.PageID, &r.Position, &r.Content, &r.PageTitle, &r.PageSlug, &r.Domain, &r.Project); err != nil {
			return nil, fmt.Errorf("load scan: %w", err)
		}
		r.Score = scores[r.ChunkID]
		r.Source = source
		byID[r.ChunkID] = r
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Preserva ordem de `ids` (que veio do RRF/single-channel)
	out := make([]Result, 0, len(ids))
	for _, id := range ids {
		if r, ok := byID[id]; ok {
			out = append(out, r)
		}
	}
	return out, nil
}

// loadChunkTimestamps retorna map[chunk_id]created_at pra um conjunto de
// chunk IDs. Usado pelo recency boost — load separado pra não engordar
// loadResults (que já carrega title/slug/content).
func loadChunkTimestamps(ctx context.Context, tx pgx.Tx, ids []int64) (map[int64]time.Time, error) {
	if len(ids) == 0 {
		return map[int64]time.Time{}, nil
	}
	rows, err := tx.Query(ctx, `SELECT id, created_at FROM chunks WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("load timestamps: %w", err)
	}
	defer rows.Close()
	out := make(map[int64]time.Time, len(ids))
	for rows.Next() {
		var id int64
		var t time.Time
		if err := rows.Scan(&id, &t); err != nil {
			return nil, err
		}
		out[id] = t
	}
	return out, rows.Err()
}

// loadDecayExemptChunks retorna o set de chunk IDs cuja PAGE tem ao menos
// uma entity de kind preference|lesson linked via edge. Esses chunks
// pulam o recency decay (Sprint 1.7 — padrão OMEGA "exemptions for
// preferences and error patterns").
//
// Erro de DB não é fatal: search degrada gracioso aplicando decay em todos
// (caller já trata via fallback no map vazio).
func loadDecayExemptChunks(ctx context.Context, tx pgx.Tx, ids []int64) (map[int64]bool, error) {
	if len(ids) == 0 {
		return map[int64]bool{}, nil
	}
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT c.id
		FROM chunks c
		JOIN edges e ON e.source_page_id = c.page_id
		JOIN entities ent ON ent.id = e.from_entity_id OR ent.id = e.to_entity_id
		WHERE c.id = ANY($1)
		  AND ent.kind IN ('preference', 'lesson')
		  AND e.valid_to IS NULL
	`, ids)
	if err != nil {
		return map[int64]bool{}, fmt.Errorf("load decay-exempt chunks: %w", err)
	}
	defer rows.Close()
	out := make(map[int64]bool, len(ids))
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return out, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// intToParam converte índice numérico em "$N" string. Helper pra montar SQL dinâmico.
func intToParam(n int) string {
	// Pequeno: 1-9 — não precisa strconv (otimização micro)
	if n >= 1 && n <= 9 {
		return string('$') + string('0'+rune(n))
	}
	// Fallback genérico
	return fmt.Sprintf("$%d", n)
}

// dateSearch retorna chunks cujo canal de datas (dates_tsv — tokens canônicos
// 'dYYYYMMDD', migration 0022) casa com a tsquery de datas derivada da pergunta.
// Canal DETERMINÍSTICO (config 'simple', token alfanumérico fica inteiro): recupera
// o chunk da data exata mesmo quando vetor/FTS não pegam (formatos/idiomas diferentes
// — "01 de junho de 2026" vs "2026.06.01"). É a peça que faltava no balde temporal.
func dateSearch(ctx context.Context, tx pgx.Tx, dateQuery string, limit int, domain, project string) ([]int64, error) {
	if dateQuery == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 50
	}
	q := `
		SELECT c.id
		FROM chunks c
		JOIN pages p ON p.id = c.page_id
		WHERE c.dates_tsv @@ to_tsquery('simple', $1)`
	args := []any{dateQuery}
	if domain != "" {
		args = append(args, domain)
		q += ` AND p.domain = ` + intToParam(len(args))
	}
	if project != "" {
		// Memória por projeto: traz o projeto + as globais (project IS NULL). F0.
		args = append(args, project)
		q += ` AND (p.project = ` + intToParam(len(args)) + ` OR p.project IS NULL)`
	}
	q += ` ORDER BY ts_rank(c.dates_tsv, to_tsquery('simple', $1)) DESC LIMIT ` + intToParam(len(args)+1)
	args = append(args, limit)

	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("date search: %w", err)
	}
	defer rows.Close()
	out := make([]int64, 0, limit)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("date scan: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
