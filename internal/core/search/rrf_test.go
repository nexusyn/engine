package search

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFuseRRF_SingleList(t *testing.T) {
	// Lista única → ordem preservada
	got := FuseRRF([][]int64{{10, 20, 30}}, 3)
	assert.Equal(t, []int64{10, 20, 30}, got)
}

func TestFuseRRF_EmptyLists(t *testing.T) {
	assert.Empty(t, FuseRRF(nil, 5))
	assert.Empty(t, FuseRRF([][]int64{{}}, 5))
}

func TestFuseRRF_LimitZero(t *testing.T) {
	got := FuseRRF([][]int64{{1, 2, 3}}, 0)
	assert.Empty(t, got)
}

func TestFuseRRF_TwoListsOverlap(t *testing.T) {
	// Tanto 10 quanto 20 aparecem em ambas as listas.
	// 10: rank 1 em vec (1/61) + rank 3 em fts (1/63) = 0.03226
	// 20: rank 2 em vec (1/62) + rank 2 em fts (1/62) = 0.03226
	// Mesma soma → tie-break por ID menor (10 primeiro).
	vector := []int64{10, 20, 30, 40}
	fts := []int64{50, 20, 10, 60}
	got := FuseRRF([][]int64{vector, fts}, 3)

	// Os 2 primeiros têm que ser 10 e 20 (em alguma ordem com tie-break por ID).
	require := assert.New(t)
	require.Contains([]int64{10, 20}, got[0])
	require.Contains([]int64{10, 20}, got[1])
	require.NotEqual(got[0], got[1])
}

func TestFuseRRF_TwoListsDisjoint(t *testing.T) {
	vector := []int64{1, 2, 3}
	fts := []int64{10, 20, 30}
	got := FuseRRF([][]int64{vector, fts}, 4)

	require := assert.New(t)
	require.Len(got, 4)
	// Top picks são os rank-1 de cada canal: 1 e 10
	require.Contains([]int64{1, 10}, got[0])
	require.Contains([]int64{1, 10}, got[1])
	require.NotEqual(got[0], got[1])
}

func TestFuseRRF_DeterministicTieBreak(t *testing.T) {
	// Mesmos IDs em mesma posição → tie. Tie-break: ID menor primeiro
	listA := []int64{30, 20, 10}
	listB := []int64{30, 20, 10}
	got := FuseRRF([][]int64{listA, listB}, 3)
	// Top 3 esperado: 30 (rank 1 ambos), 20 (rank 2 ambos), 10 (rank 3 ambos)
	assert.Equal(t, []int64{30, 20, 10}, got)
}

func TestFuseRRF_LimitGreaterThanResults(t *testing.T) {
	got := FuseRRF([][]int64{{1, 2}}, 10)
	assert.Equal(t, []int64{1, 2}, got)
}

func TestScoreOf(t *testing.T) {
	lists := [][]int64{{1, 2, 3}, {3, 2, 1}}
	// id=2 aparece em rank 2 em ambas → 2 * 1/(60+2) = ~0.0323
	s := ScoreOf(lists, 2)
	expected := 2.0 / 62.0
	assert.InDelta(t, expected, s, 0.0001)

	// id=99 não está em nenhuma lista → 0
	assert.Zero(t, ScoreOf(lists, 99))
}
