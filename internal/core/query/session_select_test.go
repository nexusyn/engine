package query

import (
	"testing"

	"github.com/nexusyn/engine/internal/core/search"
)

// TestSessionLevelSelect_CoversSparseSessions garante o objetivo do multi-session
// recall: dada uma sessão dominante (verbosa, muitos chunks) e sessões esparsas
// (1 evento cada), a seleção por-sessão deve SURFAR as esparsas — não encher o
// orçamento só com a dominante (que é o que o top-K puro faria).
func TestSessionLevelSelect_CoversSparseSessions(t *testing.T) {
	var results []search.Result
	// Sessão dominante (page 1): 10 chunks de score alto.
	for i := 0; i < 10; i++ {
		results = append(results, search.Result{ChunkID: int64(i), PageID: 1, Score: 0.90 - float64(i)*0.01})
	}
	// Sessões esparsas (pages 2,3,4): 1 chunk cada, score menor.
	results = append(results,
		search.Result{ChunkID: 100, PageID: 2, Score: 0.50},
		search.Result{ChunkID: 101, PageID: 3, Score: 0.45},
		search.Result{ChunkID: 102, PageID: 4, Score: 0.40},
	)

	out := sessionLevelSelect(results, sessionTopP, 4)

	pages := map[int64]bool{}
	for _, r := range out {
		pages[r.PageID] = true
	}
	if len(pages) != 4 {
		t.Fatalf("esperava cobertura das 4 sessões, obtive %d (pages=%v)", len(pages), pages)
	}
	if len(out) != 4 {
		t.Fatalf("esperava 4 chunks (budget), obtive %d", len(out))
	}
}

// TestSessionLevelSelect_RanksByNDCG verifica que uma sessão com vários chunks
// relevantes ranqueia acima de uma com um único chunk de score similar.
func TestSessionLevelSelect_RanksByNDCG(t *testing.T) {
	results := []search.Result{
		{ChunkID: 1, PageID: 10, Score: 0.6},
		{ChunkID: 2, PageID: 10, Score: 0.55}, // page 10 tem 2 chunks fortes
		{ChunkID: 3, PageID: 20, Score: 0.6},  // page 20 tem 1 chunk
	}
	out := sessionLevelSelect(results, 1, 1) // top-1 sessão, 1 chunk
	if len(out) != 1 || out[0].PageID != 10 {
		t.Fatalf("esperava top-1 sessão = page 10 (maior NDCG), obtive %+v", out)
	}
}

// TestSessionLevelSelect_Passthrough: <=1 resultado retorna como veio.
func TestSessionLevelSelect_Passthrough(t *testing.T) {
	one := []search.Result{{ChunkID: 1, PageID: 1, Score: 0.5}}
	if got := sessionLevelSelect(one, 8, 10); len(got) != 1 {
		t.Fatalf("passthrough esperado, obtive %d", len(got))
	}
}
