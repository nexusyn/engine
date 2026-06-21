package query

import (
	"testing"

	"github.com/nexusyn/engine/internal/core/search"
)

func TestDiversifyByPage_CapPorPagina(t *testing.T) {
	in := []search.Result{
		{ChunkID: 1, PageID: 10}, {ChunkID: 2, PageID: 10}, {ChunkID: 3, PageID: 10},
		{ChunkID: 4, PageID: 20}, {ChunkID: 5, PageID: 10}, {ChunkID: 6, PageID: 20},
	}
	out := diversifyByPage(in, 2)
	perPage := map[int64]int{}
	for _, r := range out {
		perPage[r.PageID]++
	}
	if perPage[10] != 2 || perPage[20] != 2 {
		t.Fatalf("cap por página falhou: %v", perPage)
	}
	// preserva ordem de relevância: primeiros chunks de cada página entram.
	if out[0].ChunkID != 1 || out[1].ChunkID != 2 {
		t.Errorf("ordem não preservada: %+v", out)
	}
}

func TestDiversifyByPage_NoOpQuandoDesligado(t *testing.T) {
	in := []search.Result{{ChunkID: 1, PageID: 10}, {ChunkID: 2, PageID: 10}}
	if got := diversifyByPage(in, 0); len(got) != len(in) {
		t.Errorf("max=0 deveria ser no-op; veio %d", len(got))
	}
}
