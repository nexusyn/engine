package query

import (
	"regexp"
	"time"

	"github.com/nexusyn/engine/internal/core/search"
)

// Sprint multi-session-recall — count/aggregation queries precisam de RECALL
// (todos os eventos espalhados em sessions distintas), não precisão top-K.
//
// Padrão observado no bench LongMemEval: "How many plants did I acquire?" gold
// 3 → sistema responde 2 (pega N-1 de N porque o N-ésimo chunk fica fora do
// top-5). Aumentar o limit pra count queries dá ao LLM cobertura de todos os
// chunks relevantes pra somar/contar corretamente.
var countQueryRegex = regexp.MustCompile(
	`(?i)\b(` +
		// EN — interrogativos de contagem/frequência
		`how many|how much|how often|number of|count of|` +
		// EN — agregação por TOTAL/SOMA. "total" seguido de QUALQUER medida
		// (weight/distance/duration/amount/cost/hours/…), não só number/count.
		// Captura "What is the total weight of the feed I bought" (antes escapava
		// e undercontava: 50 vs 70). Também combined/altogether/in total/all together.
		`total\s+\w+|combined|altogether|in total|all together|` +
		// PT-BR
		`quant[oa]s|quantas vezes|n[úu]mero de|total de|no total|ao todo|com que frequ[êe]ncia` +
		`)\b`,
)

// IsCountQuery detecta perguntas de contagem/agregação. Quando true, o pipeline
// expande o limit interno pra priorizar recall (cobertura de todos os eventos)
// sobre precisão (top-K mais relevantes).
func IsCountQuery(q string) bool {
	return countQueryRegex.MatchString(q)
}

// RecallLimit é o limit pra queries que precisam de RECALL: count/agregação E
// ordenação/duração temporal (tudo que é NeedsReasoning). Diagnóstico n=350
// (2026-05-26): temporal (78.8%) e multi-session (74.2%) falham por RECALL —
// o LLM conta/ordena certo quando tem os dados, mas eventos espalhados em
// sessions distintas ficam fora do top-K (ex: "order of airlines" → achou 2 de
// 4). 40 cobre ~20-40 sessions; gemini-3.5 (1M ctx) absorve sem problema.
const RecallLimit = 40

// EffectiveLimit retorna o limit a usar dado o limit pedido e o tipo de query.
// Queries de raciocínio (count/ordenação/duração) sobem pra RecallLimit pra
// cobrir todos os eventos. Recomendação/preferência TAMBÉM sobe: o fato que
// fundamenta a sugestão ("cultivo tomate-cereja", "tenho Suica + TripIt", "sou
// iPhone 13 Pro", "cansei de true crime") é semanticamente DISTANTE da pergunta
// ("o que servir no jantar?") → cai fora do top-20 → o modelo refusa/responde
// genérico. Mais recall traz o specific pro contexto; o reranker (que NÃO é
// pulado em preference) reordena, então recall sobe sem perder precisão.
func EffectiveLimit(question string, requested int) int {
	if (NeedsReasoning(question) || IsPreferenceQuery(question)) && requested < RecallLimit {
		return RecallLimit
	}
	return requested
}

// reasoningQueryRegex cobre agregação/superlativo/estado-atual — queries onde
// ENUMERAR as instâncias/menções antes de responder reduz undercount e
// valor-stale (ex: "which store did I spend most", "current record",
// "to-watch list"). Bench 2026-05-24: multi-session e knowledge-update travam
// ~69% em todos os modelos porque o LLM subconta instâncias dispersas.
var reasoningQueryRegex = regexp.MustCompile(
	`(?i)(` +
		// agregação / superlativo / estado-atual
		`\bwhich\b.{0,40}\b(most|least|best|top|highest|lowest)\b|` +
		`\b(most|least)\b.{0,30}\b(money|spend|spent|time|often|frequent)\b|` +
		`\bcurrent(ly)?\b|\brecord\b|\blatest\b|\bto-?watch\b|` +
		// ordering / sequência (enumerar eventos + ordenar por [Session date])
		`\border of\b|\bin what order\b|\bwhich\b.{0,40}\b(first|earliest|latest)\b|` +
		`happened first|came first|\bwho\b.{0,30}\b(first|second|third)\b|` +
		// duração / delta entre datas
		`\bhow long\b.{0,30}\b(before|since|been)\b|` +
		`\bhow many (weeks?|months?|days?|years?|hours?)\b.{0,45}\b(since|before|passed|ago|between)\b|` +
		// PT-BR
		`qual.{0,40}\b(mais|menos|maior|menor)\b|atual(mente)?|recorde|qual a ordem|quem .{0,20}primeiro` +
		`)`,
)

// ReasoningBudget é o thinkingBudget (tokens) habilitado em queries de
// count/agregação. 0 (default) = resposta direta; >0 deixa o Gemini enumerar
// internamente antes de contar/resolver. Só o provider Gemini consome isso.
const ReasoningBudget = 8192

// NeedsReasoning: count OU agregação/superlativo/estado-atual. Quando true, o
// query path liga thinking no Gemini pra enumerar antes de responder.
func NeedsReasoning(q string) bool {
	return IsCountQuery(q) || reasoningQueryRegex.MatchString(q)
}

// temporalQueryRegex detecta queries de RACIOCÍNIO TEMPORAL puro: ordenação de
// eventos por data + duração/delta entre datas. NÃO inclui count nem
// "current/latest" (estado-atual) — esses são knowledge-update, onde o M3 é
// FORTE (93.8% no bench). O alvo aqui é só o tipo temporal-reasoning, onde o M3
// é lento e estoura timeout (date-math chegou a 107s) → roteado pro M2.7.
var temporalQueryRegex = regexp.MustCompile(
	`(?i)(` +
		// ordenação / sequência cronológica
		`\border of\b|\bin what order\b|\bearliest\b|\bchronological\b|` +
		`\bcame (first|last|before|after)\b|happened (first|last|before|after)|` +
		`\bwhich\b.{0,40}\b(first|second|third|earliest)\b|` +
		// duração / delta entre datas
		`\bhow long\b|` +
		`\bhow many (days?|weeks?|months?|years?|hours?|minutes?)\b.{0,45}\b(since|before|after|ago|passed|between|until|till)\b|` +
		`\bdays?\b.{0,15}\b(since|before|after|between|until)\b|` +
		// PT-BR
		`qual a ordem|quem .{0,20}primeiro|h[áa] quanto tempo|quantos? (dias|semanas|meses|anos)\b.{0,30}\b(desde|antes|atr[áa]s|entre)` +
		`)`,
)

// IsTemporalQuery: a query exige raciocínio temporal (ordenação/duração/data
// específica)? Usado pelo roteador de modelo híbrido — temporais vão pro modelo
// rápido/confiável (M2.7) em vez do default (M3, lento em date-math). Combina o
// regex de ordenação/duração com os detectores de date-math e date-anchor já
// existentes ("between/since/ago + datas", "what did I do on March 15").
func IsTemporalQuery(q string) bool {
	if temporalQueryRegex.MatchString(q) {
		return true
	}
	if IsDateMathQuery(q) {
		return true
	}
	return !search.DetectDateAnchor(q, time.Now().UTC()).IsZero()
}
