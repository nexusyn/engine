package search

import (
	"math"
	"os"
	"strconv"
	"time"
)

// recencyExemptFloor é o PISO de decay aplicado aos durables (preference/lesson).
// Histórico: isenção TOTAL (= 1.0) — durável nunca perdia score por idade, o que
// deixava um durável ENVENENADO dominar o topo pra sempre. Agora o piso limita isso:
// o durável decai como qualquer chunk, mas nunca abaixo deste valor. 1.0 = comportamento
// antigo (isenção total); 0.0 = sem exemption. Tunável por env, revert instantâneo.
var recencyExemptFloor = envFloatOr("NEXUS_RECENCY_EXEMPT_FLOOR", 0.5)

func envFloatOr(k string, def float64) float64 {
	if v := os.Getenv(k); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 && f <= 1 {
			return f
		}
	}
	return def
}

// ApplyRecencyBoost re-scoreia chunks via decay exponencial sobre idade.
//
// Para cada chunkID em scores:
//
//	new_score = old_score * exp(-age_days / halfLifeDays)
//
// Ou seja: chunk com idade = halfLife reduz pra 1/e ≈ 0.37x. Idade 0 = 1x.
//
// Motivação Day 22: bench LongMemEval `knowledge-update` falhou ao
// recuperar chunk antigo ("6 months") em vez do mais novo ("9 months")
// — mesma similaridade semântica, mas o recente é o correto. Boost
// resolve sem schema change.
//
// halfLifeDays <= 0 desativa (no-op). createdAt vazio também — sem dados
// de timestamp, score original mantido.
//
// Sprint 1.7 (2026-05-21): exempt é set opcional de chunk_ids isentos do
// decay (preferences + lessons — fatos durables que NÃO devem perder
// score por idade). Padrão OMEGA: "0.35 floor, exemptions for preferences
// and error patterns". Sem schema change — chamado decide via lookup
// auxiliar de entity kinds.
func ApplyRecencyBoost(scores map[int64]float64, createdAt map[int64]time.Time, halfLifeDays float64, now time.Time, exempt map[int64]bool, exemptFloor float64) map[int64]float64 {
	if halfLifeDays <= 0 || len(createdAt) == 0 {
		return scores
	}
	out := make(map[int64]float64, len(scores))
	for id, score := range scores {
		t, ok := createdAt[id]
		if !ok {
			out[id] = score
			continue
		}
		ageDays := now.Sub(t).Hours() / 24.0
		if ageDays < 0 {
			ageDays = 0 // chunk futuro (clock skew): trata como novo
		}
		multiplier := math.Exp(-ageDays / halfLifeDays)
		// Durável (preference/lesson): decai como qualquer chunk, mas nunca abaixo do
		// piso (exemptFloor=1.0 → isenção total, comportamento histórico).
		if exempt[id] && multiplier < exemptFloor {
			multiplier = exemptFloor
		}
		out[id] = score * multiplier
	}
	return out
}

// ReorderByScore retorna ids ordenados pelos scores DESC. Usado depois
// de ApplyRecencyBoost pra re-ranquear sem precisar passar pelo RRF de novo.
func ReorderByScore(ids []int64, scores map[int64]float64) []int64 {
	out := make([]int64, len(ids))
	copy(out, ids)
	sortByScoreDesc(out, scores)
	return out
}
