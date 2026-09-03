package search

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/nexusyn/engine/internal/provider/rerank"
)

// reorderByRanked traduz o resultado do reranker (Índice na lista docs) de volta
// pros chunk ids, na ordem ranqueada. É o núcleo do rerank pré-fusão do grafo
// (Módulo B) — contrato: Index → kept[Index], fora-do-range ignorado.
func TestReorderByRanked(t *testing.T) {
	kept := []int64{10, 20, 30}

	// reranker diz: doc idx 2 é o + relevante, depois idx 0, idx 1.
	ranked := []rerank.Result{{Index: 2, Score: 0.9}, {Index: 0, Score: 0.5}, {Index: 1, Score: 0.1}}
	assert.Equal(t, []int64{30, 10, 20}, reorderByRanked(kept, ranked), "reordena pela ordem do reranker")

	// índices fora do range são ignorados (defensivo — nunca panica).
	assert.Equal(t, []int64{20}, reorderByRanked(kept, []rerank.Result{{Index: 1}, {Index: 99}, {Index: -1}}))

	// vazio entra, vazio sai.
	assert.Empty(t, reorderByRanked(kept, nil))

	// top-N parcial: reranker devolve só os 2 melhores → só esses 2 saem.
	assert.Equal(t, []int64{30, 10}, reorderByRanked(kept, []rerank.Result{{Index: 2}, {Index: 0}}))
}
