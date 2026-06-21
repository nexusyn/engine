package search

// rrfK é o constante k do Reciprocal Rank Fusion (literatura: ~60).
// Score = sum(weight * 1 / (k + rank_in_channel)) across all channels.
const rrfK = 60

// EntityMatchWeight é o peso do canal entity-match no RRF. >1 dá um boost às
// memórias cuja entidade casa com a query (proper nouns/conceitos), espelhando
// o "entity-matched memories receive a ranking boost" da literatura.
// Vector e FTS ficam em 1.0 (baseline). Ajustável; validar no bench antes de mexer.
const EntityMatchWeight = 1.5

// DateMatchWeight — canal de data (0022) é determinístico e de alta precisão →
// peso forte pra garantir que o chunk da data exata entra no top.
const DateMatchWeight = 0.5

// GraphExpansionWeight — canal de graph-expansion (Fase 1). Peso CONSERVADOR:
// é o sinal mais indireto (memórias a 1-2 hops dos seeds), entra como reforço,
// não como driver. Sob NEXUS_GRAPH_EXPANSION; validar no bench antes de subir.
const GraphExpansionWeight = 0.8

// weightAt retorna o peso do canal i (1.0 se weights nil/curto) — deixa os
// callers antigos (FuseRRF/ScoreOf) reusarem o núcleo ponderado sem mudança.
func weightAt(weights []float64, i int) float64 {
	if i < len(weights) {
		return weights[i]
	}
	return 1.0
}

// FuseRRFWeighted combina N listas ordenadas (relevância DESC) em ranking único,
// cada canal com um peso. Score = sum(weight_canal / (k + rank)).
func FuseRRFWeighted(lists [][]int64, weights []float64, limit int) []int64 {
	if limit <= 0 {
		return nil
	}
	scores := make(map[int64]float64)
	for li, list := range lists {
		w := weightAt(weights, li)
		for rank, id := range list {
			scores[id] += w / float64(rrfK+rank+1) // rank 1-based
		}
	}

	// Sort by score DESC, deterministic tie-break by ID
	ids := make([]int64, 0, len(scores))
	for id := range scores {
		ids = append(ids, id)
	}
	sortByScoreDesc(ids, scores)

	if limit > len(ids) {
		limit = len(ids)
	}
	return ids[:limit]
}

// FuseRRF é o RRF clássico (todos os canais com peso 1.0).
//
// Por que RRF e não score normalization:
//   - Scores de vector (cosine distance) e FTS (ts_rank) têm escalas
//     diferentes — normalizar é frágil
//   - RRF usa apenas ranking, não score absoluto — robusto
//   - k=60 é literatura padrão (Cormack et al., 2009)
func FuseRRF(lists [][]int64, limit int) []int64 {
	return FuseRRFWeighted(lists, nil, limit)
}

// ScoreOfWeighted retorna o score RRF ponderado de um id (pra exposição/debug).
func ScoreOfWeighted(lists [][]int64, weights []float64, id int64) float64 {
	var sum float64
	for li, list := range lists {
		w := weightAt(weights, li)
		for rank, x := range list {
			if x == id {
				sum += w / float64(rrfK+rank+1)
				break
			}
		}
	}
	return sum
}

// ScoreOf retorna o score RRF de um id específico (pesos 1.0).
func ScoreOf(lists [][]int64, id int64) float64 {
	return ScoreOfWeighted(lists, nil, id)
}

// sortByScoreDesc faz insertion sort simples (datasets pequenos no NEXUS).
// Para >1000 ids preferir sort.SliceStable, mas pra limit=10-50 isso é OK.
func sortByScoreDesc(ids []int64, scores map[int64]float64) {
	for i := 1; i < len(ids); i++ {
		j := i
		for j > 0 && (scores[ids[j]] > scores[ids[j-1]] ||
			(scores[ids[j]] == scores[ids[j-1]] && ids[j] < ids[j-1])) {
			ids[j], ids[j-1] = ids[j-1], ids[j]
			j--
		}
	}
}
