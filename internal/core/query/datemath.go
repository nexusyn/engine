package query

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DateMathFact é um cálculo determinístico de tempo extraído da pergunta.
// Renderizado no prompt como bloco COMPUTED FACTS antes da QUESTION pra
// o LLM usar o número verbatim em vez de fazer date math em texto (onde
// erra com frequência — Sprint 2.3 do road-to-95).
type DateMathFact struct {
	Description string
}

var (
	betweenRegex = regexp.MustCompile(`(?i)\b(between|entre)\b`)
	sinceRegex   = regexp.MustCompile(`(?i)\b(since|desde)\b|\bh[áa] quanto\b|\bhow (long|many days|many weeks|many months|many years)\b`)
	agoRegex     = regexp.MustCompile(`(?i)\b(ago|atr[áa]s)\b`)
	fromNowRegex = regexp.MustCompile(`(?i)\b(from now|until|daqui a|at[eé])\b`)

	isoDateRegex   = regexp.MustCompile(`\b(\d{4})[-/](\d{1,2})[-/](\d{1,2})\b`)
	monthDayRegex  = regexp.MustCompile(`(?i)\b(january|february|march|april|may|june|july|august|september|october|november|december|jan|feb|mar|apr|jun|jul|aug|sep|sept|oct|nov|dec)\s+(\d{1,2})(?:[,\s]+(\d{4}))?\b`)
	diaDeMesRegex  = regexp.MustCompile(`(?i)\b(\d{1,2})\s+de\s+(janeiro|fevereiro|mar[çc]o|abril|maio|junho|julho|agosto|setembro|outubro|novembro|dezembro)(?:\s+de\s+(\d{4}))?\b`)
	relativeRegex  = regexp.MustCompile(`(?i)\b(\d+)\s+(day|days|week|weeks|month|months|year|years|dia|dias|semana|semanas|m[eê]s|meses|ano|anos)\s+(ago|atr[áa]s)\b`)
	weekdayBackRef = regexp.MustCompile(`(?i)\b(last|past|[uú]ltim[oa])\s+(monday|tuesday|wednesday|thursday|friday|saturday|sunday|segunda|ter[çc]a|quarta|quinta|sexta|s[áa]bado|domingo)\b`)
)

var monthMapEN = map[string]time.Month{
	"january": 1, "jan": 1,
	"february": 2, "feb": 2,
	"march": 3, "mar": 3,
	"april": 4, "apr": 4,
	"may":  5,
	"june": 6, "jun": 6,
	"july": 7, "jul": 7,
	"august": 8, "aug": 8,
	"september": 9, "sep": 9, "sept": 9,
	"october": 10, "oct": 10,
	"november": 11, "nov": 11,
	"december": 12, "dec": 12,
}

var monthMapPT = map[string]time.Month{
	"janeiro":   1,
	"fevereiro": 2,
	"março":     3, "marco": 3,
	"abril":    4,
	"maio":     5,
	"junho":    6,
	"julho":    7,
	"agosto":   8,
	"setembro": 9,
	"outubro":  10,
	"novembro": 11,
	"dezembro": 12,
}

var weekdayMap = map[string]time.Weekday{
	"sunday":    time.Sunday,
	"monday":    time.Monday,
	"tuesday":   time.Tuesday,
	"wednesday": time.Wednesday,
	"thursday":  time.Thursday,
	"friday":    time.Friday,
	"saturday":  time.Saturday,
	"domingo":   time.Sunday,
	"segunda":   time.Monday,
	"terça":     time.Tuesday, "terca": time.Tuesday,
	"quarta": time.Wednesday,
	"quinta": time.Thursday,
	"sexta":  time.Friday,
	"sábado": time.Saturday, "sabado": time.Saturday,
}

// IsDateMathQuery é o gate público — true se a pergunta plausivelmente envolve
// date arithmetic. Liberal: falso positivo só anexa bloco vazio (extractDates
// pode não achar nada e ComputeDateMath retorna nil).
func IsDateMathQuery(q string) bool {
	return betweenRegex.MatchString(q) ||
		sinceRegex.MatchString(q) ||
		agoRegex.MatchString(q) ||
		fromNowRegex.MatchString(q)
}

// ComputeDateMath retorna fatos determinísticos pra injetar no prompt.
// Retorna nil quando nenhuma date math segura pode ser feita (e.g. menos
// datas que o intent pede, ou intent não detectado).
func ComputeDateMath(question string, now time.Time) []DateMathFact {
	if !IsDateMathQuery(question) {
		return nil
	}
	// Normaliza pra meia-noite UTC pra date-math estável (evita arredondamento
	// de fração de dia quando now traz horas).
	now = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	dates := extractDates(question, now)
	if rel := resolveRelative(question, now); !rel.IsZero() {
		dates = append(dates, rel)
	}
	if wd := resolveWeekdayBack(question, now); !wd.IsZero() {
		dates = append(dates, wd)
	}
	dates = uniqDates(dates)

	hasBetween := betweenRegex.MatchString(question)
	hasSinceOrAgo := sinceRegex.MatchString(question) || agoRegex.MatchString(question)
	hasFromNow := fromNowRegex.MatchString(question)

	var facts []DateMathFact
	switch {
	case hasBetween && len(dates) >= 2:
		// pares (limita a 3 pares pra não inundar prompt)
		count := 0
		for i := 0; i < len(dates); i++ {
			for j := i + 1; j < len(dates); j++ {
				facts = append(facts, formatBetween(dates[i], dates[j]))
				count++
				if count >= 3 {
					return facts
				}
			}
		}
	case (hasSinceOrAgo || hasFromNow) && len(dates) >= 1:
		for _, d := range dates {
			facts = append(facts, formatSince(d, now))
		}
	case len(dates) >= 2:
		// fallback: duas datas presentes mas intent ambíguo → assume between
		facts = append(facts, formatBetween(dates[0], dates[1]))
	}
	return facts
}

// FormatDateMathFacts renderiza fatos como bloco markdown pra prompt.
func FormatDateMathFacts(facts []DateMathFact) string {
	if len(facts) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("COMPUTED FACTS (deterministic — use these numbers verbatim, do not recompute):\n")
	for _, f := range facts {
		b.WriteString("- ")
		b.WriteString(f.Description)
		b.WriteString("\n")
	}
	return b.String()
}

func formatBetween(a, b time.Time) DateMathFact {
	if a.After(b) {
		a, b = b, a
	}
	days := int(b.Sub(a) / (24 * time.Hour))
	desc := fmt.Sprintf("%d days between %s and %s", days, a.Format("2006-01-02"), b.Format("2006-01-02"))
	switch {
	case days >= 14 && days < 60:
		desc = fmt.Sprintf("%d days (≈%d weeks) between %s and %s", days, days/7, a.Format("2006-01-02"), b.Format("2006-01-02"))
	case days >= 60:
		desc = fmt.Sprintf("%d days (≈%d months) between %s and %s", days, monthsBetween(a, b), a.Format("2006-01-02"), b.Format("2006-01-02"))
	}
	return DateMathFact{Description: desc}
}

func formatSince(d, now time.Time) DateMathFact {
	if d.After(now) {
		days := int(d.Sub(now) / (24 * time.Hour))
		return DateMathFact{Description: fmt.Sprintf("%d days from today (today: %s, target: %s)", days, now.Format("2006-01-02"), d.Format("2006-01-02"))}
	}
	days := int(now.Sub(d) / (24 * time.Hour))
	base := fmt.Sprintf("%d days since %s (today: %s)", days, d.Format("2006-01-02"), now.Format("2006-01-02"))
	switch {
	case days >= 14 && days < 60:
		base = fmt.Sprintf("%d days (≈%d weeks) since %s (today: %s)", days, days/7, d.Format("2006-01-02"), now.Format("2006-01-02"))
	case days >= 60:
		base = fmt.Sprintf("%d days (≈%d months) since %s (today: %s)", days, monthsBetween(d, now), d.Format("2006-01-02"), now.Format("2006-01-02"))
	}
	return DateMathFact{Description: base}
}

func monthsBetween(a, b time.Time) int {
	if a.After(b) {
		a, b = b, a
	}
	months := (b.Year()-a.Year())*12 + int(b.Month()-a.Month())
	if b.Day() < a.Day() {
		months--
	}
	if months < 0 {
		months = 0
	}
	return months
}

func extractDates(q string, now time.Time) []time.Time {
	var dates []time.Time

	for _, m := range isoDateRegex.FindAllStringSubmatch(q, -1) {
		y, _ := strconv.Atoi(m[1])
		mo, _ := strconv.Atoi(m[2])
		d, _ := strconv.Atoi(m[3])
		if validYMD(y, mo, d) {
			dates = append(dates, time.Date(y, time.Month(mo), d, 0, 0, 0, 0, time.UTC))
		}
	}

	for _, m := range monthDayRegex.FindAllStringSubmatch(q, -1) {
		mo, ok := monthMapEN[strings.ToLower(m[1])]
		if !ok {
			continue
		}
		d, _ := strconv.Atoi(m[2])
		y := now.Year()
		if m[3] != "" {
			y, _ = strconv.Atoi(m[3])
		}
		if validYMD(y, int(mo), d) {
			dates = append(dates, time.Date(y, mo, d, 0, 0, 0, 0, time.UTC))
		}
	}

	for _, m := range diaDeMesRegex.FindAllStringSubmatch(q, -1) {
		mo, ok := monthMapPT[strings.ToLower(m[2])]
		if !ok {
			continue
		}
		d, _ := strconv.Atoi(m[1])
		y := now.Year()
		if m[3] != "" {
			y, _ = strconv.Atoi(m[3])
		}
		if validYMD(y, int(mo), d) {
			dates = append(dates, time.Date(y, mo, d, 0, 0, 0, 0, time.UTC))
		}
	}

	return dates
}

// resolveRelative captura "N days ago" / "N semanas atrás" → data absoluta.
func resolveRelative(q string, now time.Time) time.Time {
	m := relativeRegex.FindStringSubmatch(q)
	if m == nil {
		return time.Time{}
	}
	n, _ := strconv.Atoi(m[1])
	unit := strings.ToLower(m[2])
	switch unit {
	case "day", "days", "dia", "dias":
		return now.AddDate(0, 0, -n)
	case "week", "weeks", "semana", "semanas":
		return now.AddDate(0, 0, -7*n)
	case "month", "months", "mês", "mes", "meses":
		return now.AddDate(0, -n, 0)
	case "year", "years", "ano", "anos":
		return now.AddDate(-n, 0, 0)
	}
	return time.Time{}
}

// resolveWeekdayBack captura "last Monday" / "última segunda" → data absoluta.
func resolveWeekdayBack(q string, now time.Time) time.Time {
	m := weekdayBackRef.FindStringSubmatch(q)
	if m == nil {
		return time.Time{}
	}
	target, ok := weekdayMap[strings.ToLower(m[2])]
	if !ok {
		return time.Time{}
	}
	diff := int(now.Weekday() - target)
	if diff <= 0 {
		diff += 7
	}
	return now.AddDate(0, 0, -diff)
}

func validYMD(y, m, d int) bool {
	if y < 1900 || y > 2200 {
		return false
	}
	if m < 1 || m > 12 {
		return false
	}
	if d < 1 || d > 31 {
		return false
	}
	t := time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC)
	return t.Year() == y && t.Month() == time.Month(m) && t.Day() == d
}

func uniqDates(in []time.Time) []time.Time {
	seen := make(map[string]struct{}, len(in))
	out := make([]time.Time, 0, len(in))
	for _, t := range in {
		k := t.Format("2006-01-02")
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, t)
	}
	return out
}
