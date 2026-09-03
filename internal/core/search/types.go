// Package search implementa retrieval híbrido (vector + FTS + RRF fusion).
//
// Filosofia:
//   - chunks são a unidade (não pages) — granularidade fina pra recall
//   - 3 sinais: semantic (vector), lexical (FTS pt-BR), graph (entity match — Day 10)
//   - RRF fusion combina ranking sem precisar normalizar scores entre sinais
//   - Reranker (Jina rerank) re-ordena top-K final
package search

import "time"

// Mode controla quais sinais a query usa.
type Mode string

const (
	ModeHybrid Mode = "hybrid" // default: vector + FTS via RRF
	ModeVector Mode = "vector" // só semantic
	ModeFTS    Mode = "fts"    // só lexical
)

// Options parametriza uma busca.
type Options struct {
	Query   string // texto da pergunta/busca
	Limit   int    // top-N final retornado (default 10)
	Mode    Mode   // hybrid | vector | fts
	Domain  string // filtrar por domain (memory|wiki|source|...), opcional
	Project string // filtrar por projeto na org; "" = todos. Filtrado traz projeto + globais (project IS NULL)

	// RecencyHalfLifeDays aplica boost exponencial sobre score: chunks mais
	// novos sobem no ranking conforme exp(-age_days / halfLife). Default 30d
	// (gentle decay). 0 = desligado. Útil pra knowledge-update: priorizar
	// chunk recente ("9 months") sobre antigo ("6 months") com mesma sim.
	RecencyHalfLifeDays float64

	// Diversify aplica cap por-página na seleção final (count/aggregation):
	// evita que uma conversa verbosa monopolize os slots, espalhando por mais
	// sessões pra cobrir eventos esparsos (ex: "sold/fixed furniture" em sessões
	// raras que o top-K puro abafa). Default false.
	Diversify bool

	// AsOf restringe a busca ao estado do grafo num timestamp passado
	// (time-travel — Sprint 3.2). Quando setado:
	//   - entities filtradas por valid_from <= asOf AND (valid_to IS NULL OR valid_to > asOf)
	//   - edges similar
	//   - chunks filtrados por pages.created_at <= asOf
	// Default nil = sem restrição (state current).
	AsOf *time.Time
}

// Defaults preenche valores não-setados.
func (o *Options) Defaults() {
	if o.Limit <= 0 {
		o.Limit = 10
	}
	if o.Mode == "" {
		o.Mode = ModeHybrid
	}
	if o.RecencyHalfLifeDays == 0 {
		o.RecencyHalfLifeDays = 30 // gentle decay por default
	}
}

// Result é um chunk retornado pela busca.
type Result struct {
	ChunkID   int64   `json:"chunk_id"`
	PageID    int64   `json:"page_id"`
	Position  int     `json:"position"`
	Content   string  `json:"content"`
	PageTitle string  `json:"page_title"`
	PageSlug  string  `json:"page_slug"`
	Domain    string  `json:"domain"`
	Project   string  `json:"project,omitempty"` // "" = global
	Score     float64 `json:"score"`             // score final (RRF ou raw)
	Source    string  `json:"source"` // "vector" | "fts" | "hybrid"
}
