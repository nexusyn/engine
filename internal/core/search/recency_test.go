package search

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestApplyRecencyBoost_Decays(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	scores := map[int64]float64{
		1: 1.0, // recente
		2: 1.0, // 30 dias atrás (= halfLife)
		3: 1.0, // 60 dias atrás (= 2× halfLife)
	}
	ts := map[int64]time.Time{
		1: now,
		2: now.Add(-30 * 24 * time.Hour),
		3: now.Add(-60 * 24 * time.Hour),
	}
	out := ApplyRecencyBoost(scores, ts, 30, now, nil, 1.0)

	assert.InDelta(t, 1.0, out[1], 0.001, "idade 0 = score original")
	assert.InDelta(t, math.Exp(-1), out[2], 0.001, "idade=halfLife → 1/e ≈ 0.368")
	assert.InDelta(t, math.Exp(-2), out[3], 0.001, "idade=2×halfLife → 1/e² ≈ 0.135")
}

func TestApplyRecencyBoost_DisabledWhenZero(t *testing.T) {
	now := time.Now()
	scores := map[int64]float64{1: 1.0, 2: 0.5}
	ts := map[int64]time.Time{1: now.Add(-30 * 24 * time.Hour), 2: now}
	out := ApplyRecencyBoost(scores, ts, 0, now, nil, 1.0)
	assert.Equal(t, scores, out, "halfLife=0 desativa o boost")
}

func TestApplyRecencyBoost_MissingTimestamp_KeepsScore(t *testing.T) {
	now := time.Now()
	scores := map[int64]float64{1: 1.0, 2: 0.5}
	ts := map[int64]time.Time{1: now} // só id=1
	out := ApplyRecencyBoost(scores, ts, 30, now, nil, 1.0)
	assert.InDelta(t, 1.0, out[1], 0.001)
	assert.Equal(t, 0.5, out[2], "sem timestamp = score original mantido")
}

func TestReorderByScore_ReordenaCorreto(t *testing.T) {
	ids := []int64{1, 2, 3, 4}
	scores := map[int64]float64{
		1: 0.1, // baixo — vai pro fim
		2: 0.5,
		3: 1.0, // alto — vai pro topo
		4: 0.3,
	}
	out := ReorderByScore(ids, scores)
	assert.Equal(t, []int64{3, 2, 4, 1}, out)
	// Não mutou input
	assert.Equal(t, []int64{1, 2, 3, 4}, ids)
}

func TestApplyRecencyBoost_FutureChunk_NoNegativeAge(t *testing.T) {
	// Clock skew: chunk com created_at no futuro
	now := time.Now()
	scores := map[int64]float64{1: 1.0}
	ts := map[int64]time.Time{1: now.Add(1 * time.Hour)}
	out := ApplyRecencyBoost(scores, ts, 30, now, nil, 1.0)
	assert.InDelta(t, 1.0, out[1], 0.001, "chunk no futuro tratado como idade 0")
}

func TestApplyRecencyBoost_OldVsRecent_PrefersRecent(t *testing.T) {
	// Cenário do bench: 2 chunks com mesma similaridade vetorial mas
	// recency diferente. Mais novo deve vencer após o boost.
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	scores := map[int64]float64{
		1: 0.02, // chunk "6 months" — old
		2: 0.02, // chunk "9 months" — recent (mas RRF score igual)
	}
	ts := map[int64]time.Time{
		1: now.Add(-60 * 24 * time.Hour), // 60d atrás
		2: now.Add(-7 * 24 * time.Hour),  // 7d atrás
	}
	boosted := ApplyRecencyBoost(scores, ts, 30, now, nil, 1.0)
	assert.Greater(t, boosted[2], boosted[1], "chunk recente vence o antigo")

	reordered := ReorderByScore([]int64{1, 2}, boosted)
	assert.Equal(t, []int64{2, 1}, reordered, "id=2 (mais novo) primeiro")
}

func TestApplyRecencyBoost_ExemptChunks_NoDecay(t *testing.T) {
	// Sprint 1.7: chunks de preference/lesson são isentos do decay.
	// Cenário: 3 chunks, mesma idade. Sem exempt, todos caem proporcional.
	// Com exempt={id=1}, só id=2 e id=3 sofrem decay.
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	scores := map[int64]float64{
		1: 1.0, // preference — exempt
		2: 1.0, // chunk regular
		3: 1.0, // chunk regular
	}
	ts := map[int64]time.Time{
		1: now.Add(-90 * 24 * time.Hour),
		2: now.Add(-90 * 24 * time.Hour),
		3: now.Add(-90 * 24 * time.Hour),
	}
	exempt := map[int64]bool{1: true}
	out := ApplyRecencyBoost(scores, ts, 30, now, exempt, 1.0)

	assert.Equal(t, 1.0, out[1], "exempt chunk mantém score full")
	assert.InDelta(t, math.Exp(-3), out[2], 0.001, "chunk regular decai 1/e³")
	assert.InDelta(t, math.Exp(-3), out[3], 0.001, "chunk regular decai 1/e³")
	assert.Greater(t, out[1], out[2], "exempt fica acima de regular após decay")
}

func TestApplyRecencyBoost_ExemptFloor_LimitaDominancia(t *testing.T) {
	// Hardening E: durável muito antigo NÃO mantém score full — decai até o piso.
	// Segurança: um durável envenenado não domina o topo pra sempre.
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	scores := map[int64]float64{1: 1.0, 2: 1.0}
	ts := map[int64]time.Time{
		1: now.Add(-90 * 24 * time.Hour), // exempt, 3 half-lives → decай natural 1/e³ ≈ 0.05
		2: now.Add(-90 * 24 * time.Hour),
	}
	exempt := map[int64]bool{1: true}

	// piso 0.5: durável decai mas não abaixo de 0.5; regular decai até 1/e³.
	out := ApplyRecencyBoost(scores, ts, 30, now, exempt, 0.5)
	assert.InDelta(t, 0.5, out[1], 0.001, "durável floored em 0.5 (não isenção total)")
	assert.InDelta(t, math.Exp(-3), out[2], 0.001, "regular decai normal")
	assert.Greater(t, out[1], out[2], "durável ainda fica acima do regular")

	// piso 0.0 = sem exemption: durável decai igual ao regular.
	out0 := ApplyRecencyBoost(scores, ts, 30, now, exempt, 0.0)
	assert.InDelta(t, math.Exp(-3), out0[1], 0.001, "piso 0 → durável sem exemption")
}
