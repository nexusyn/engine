package query

import (
	"regexp"
	"sort"
	"time"

	"github.com/nexusyn/engine/internal/core/search"
)

// sessionDateRegex captura `[Session date: YYYY/MM/DD]` ou `[Session date: YYYY-MM-DD]`
// no início do conteúdo de um chunk. Suporta também `Date:` simples no header.
//
// Sprint 1.3 — padrão OMEGA/ByteRover/Hindsight: chunks no prompt LLM precisam
// estar ordenados cronologicamente, não por score RRF. O system prompt diz
// "trust chronologically latest" mas só funciona se a numeração [1] [2] [3]
// reflitir a ordem temporal real.
var sessionDateRegex = regexp.MustCompile(
	`(?i)\[\s*(?:session\s+date|date|today)\s*:\s*(\d{4})[/\-](\d{1,2})[/\-](\d{1,2})\s*\]`,
)

// ExtractSessionDate procura cabeçalho `[Session date: YYYY/MM/DD]` no content
// do chunk. Tolerante a formato YYYY/MM/DD, YYYY-MM-DD, "Date:", "Today:".
// Retorna time zero se não achar — caller deve fallback pra outra fonte.
func ExtractSessionDate(content string) time.Time {
	m := sessionDateRegex.FindStringSubmatch(content)
	if len(m) != 4 {
		return time.Time{}
	}
	year, mo, day := parseInt(m[1]), parseInt(m[2]), parseInt(m[3])
	if year < 1900 || year > 2100 || mo < 1 || mo > 12 || day < 1 || day > 31 {
		return time.Time{}
	}
	return time.Date(year, time.Month(mo), day, 0, 0, 0, 0, time.UTC)
}

// SortByDateDesc ordena results pela data extraída do header DESC (mais
// recente primeiro). Estável: results sem data extraída mantêm posição
// relativa (vão pra depois dos com data). Mutates slice in-place.
//
// Permite o LLM aplicar a regra "trust chronologically latest" do system
// prompt — [1] passa a ser SEMPRE o mais recente.
func SortByDateDesc(results []search.Result) {
	// Pre-zip data+rank pra não invalidar índices durante sort
	type entry struct {
		result    search.Result
		date      time.Time
		origIndex int
	}
	entries := make([]entry, len(results))
	for i, r := range results {
		entries[i] = entry{result: r, date: ExtractSessionDate(r.Content), origIndex: i}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		di, dj := entries[i].date, entries[j].date
		// Ambos sem data: mantém ordem RRF original
		if di.IsZero() && dj.IsZero() {
			return false
		}
		// Apenas i sem data: j (com-data) vence
		if di.IsZero() {
			return false
		}
		// Apenas j sem data: i (com-data) vence
		if dj.IsZero() {
			return true
		}
		return di.After(dj)
	})
	for i, e := range entries {
		results[i] = e.result
	}
}

func parseInt(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return -1
		}
		n = n*10 + int(c-'0')
	}
	return n
}
