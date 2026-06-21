package dateutil

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Normalização determinística de datas pro retrieval.
//
// PROBLEMA: busca semântica borra datas e o FTS (plainto_tsquery) não casa
// "01 de junho de 2026" com "2026.06.01" / "01/06/2026" — viram tokens diferentes
// (e nomes de mês PT nem lematizam na config 'english'). Resultado: queries por
// data não recuperam as memórias certas (fraqueza do balde temporal no bench).
//
// SOLUÇÃO: extrair as datas de um texto e canonicalizar pra um token ISO único
// (YYYY-MM-DD). Esse token é (a) anexado ao texto indexado no ingest e (b) gerado
// a partir da query no retrieval — aí "1 de junho de 2026" e "2026.06.01" casam
// deterministicamente, independente de embedding/idioma.

var monthNames = map[string]int{
	// inglês (+ abreviações)
	"january": 1, "february": 2, "march": 3, "april": 4, "may": 5, "june": 6,
	"july": 7, "august": 8, "september": 9, "october": 10, "november": 11, "december": 12,
	"jan": 1, "feb": 2, "mar": 3, "apr": 4, "jun": 6, "jul": 7, "aug": 8,
	"sep": 9, "sept": 9, "oct": 10, "nov": 11, "dec": 12,
	// português (acentos removidos antes do lookup)
	"janeiro": 1, "fevereiro": 2, "marco": 3, "abril": 4, "maio": 5, "junho": 6,
	"julho": 7, "agosto": 8, "setembro": 9, "outubro": 10, "novembro": 11, "dezembro": 12,
}

var (
	// ISO-ish: 2026-06-01, 2026/06/01, 2026.06.01
	reISODate = regexp.MustCompile(`\b(\d{4})[-/.](\d{1,2})[-/.](\d{1,2})\b`)
	// dia-primeiro: 01/06/2026, 1-6-2026, 01.06.2026
	reDMYDate = regexp.MustCompile(`\b(\d{1,2})[-/.](\d{1,2})[-/.](\d{4})\b`)
	// "1 de junho de 2026", "01 junho 2026", "1 June 2026"
	reDayMonthYear = regexp.MustCompile(`\b(\d{1,2})\s+(?:de\s+)?([\p{L}]+)\.?\s+(?:de\s+)?(\d{4})\b`)
	// "June 1, 2026", "junho 1 2026"
	reMonthDayYear = regexp.MustCompile(`\b([\p{L}]+)\.?\s+(\d{1,2}),?\s+(\d{4})\b`)
)

// stripAccents remove acentos comuns do PT pro lookup de mês (março→marco).
var accentRepl = strings.NewReplacer(
	"á", "a", "à", "a", "ã", "a", "â", "a", "ç", "c", "é", "e", "ê", "e",
	"í", "i", "ó", "o", "ô", "o", "õ", "o", "ú", "u",
)

func monthFromWord(w string) (int, bool) {
	m, ok := monthNames[accentRepl.Replace(strings.ToLower(w))]
	return m, ok
}

func isoDate(y, m, d int) (string, bool) {
	if y < 1900 || y > 2200 || m < 1 || m > 12 || d < 1 || d > 31 {
		return "", false
	}
	return fmt.Sprintf("%04d-%02d-%02d", y, m, d), true
}

// CanonicalDates extrai todas as datas reconhecíveis de um texto e retorna os
// tokens ISO únicos (YYYY-MM-DD), na ordem de aparição, sem duplicatas. Datas sem
// ano explícito são ignoradas (não dá pra canonicalizar com segurança).
func CanonicalDates(text string) []string {
	if text == "" {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(iso string, ok bool) {
		if ok && !seen[iso] {
			seen[iso] = true
			out = append(out, iso)
		}
	}
	atoi := func(s string) int { n, _ := strconv.Atoi(s); return n }

	for _, m := range reISODate.FindAllStringSubmatch(text, -1) {
		add(isoDate(atoi(m[1]), atoi(m[2]), atoi(m[3])))
	}
	for _, m := range reDMYDate.FindAllStringSubmatch(text, -1) {
		add(isoDate(atoi(m[3]), atoi(m[2]), atoi(m[1])))
	}
	for _, m := range reDayMonthYear.FindAllStringSubmatch(text, -1) {
		if mon, ok := monthFromWord(m[2]); ok {
			add(isoDate(atoi(m[3]), mon, atoi(m[1])))
		}
	}
	for _, m := range reMonthDayYear.FindAllStringSubmatch(text, -1) {
		if mon, ok := monthFromWord(m[1]); ok {
			add(isoDate(atoi(m[3]), mon, atoi(m[2])))
		}
	}
	return out
}

// reContextDate casa marcadores de data-de-CONTEXTO injetados em queries, ex.
// "[Today: 2026/06/01]" (o harness do bench prependa; um cliente pode mandar algo
// parecido). NÃO é a data que a pergunta pede.
var reContextDate = regexp.MustCompile(`(?i)\[\s*today\s*:[^\]]*\]`)

// StripContextDate remove a data-de-contexto pra que o canal de data dispare só
// na data PERGUNTADA, não na data de "hoje". No-op se não houver marcador.
func StripContextDate(text string) string {
	return strings.TrimSpace(reContextDate.ReplaceAllString(text, " "))
}

// DateSearchTokens converte as datas de um texto em tokens de busca sem separador
// (d20260601) — usado pra popular chunks.dates no ingest. "" se não houver data.
func DateSearchTokens(text string) string {
	ds := CanonicalDates(text)
	if len(ds) == 0 {
		return ""
	}
	toks := make([]string, len(ds))
	for i, d := range ds {
		toks[i] = "d" + strings.ReplaceAll(d, "-", "")
	}
	return strings.Join(toks, " ")
}

// DateTSQuery monta um OR-string pra to_tsquery a partir das datas de um texto
// ("d20260601 | d20260602"); "" se não houver data. Usar com a config 'simple'.
func DateTSQuery(text string) string {
	ds := CanonicalDates(text)
	if len(ds) == 0 {
		return ""
	}
	toks := make([]string, len(ds))
	for i, d := range ds {
		toks[i] = "d" + strings.ReplaceAll(d, "-", "")
	}
	return strings.Join(toks, " | ")
}
