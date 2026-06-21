package query

import (
	"regexp"
	"strconv"
	"time"
)

// Sprint 3.2 — graph time-travel detector.
//
// /v1/query detecta queries que perguntam sobre estado PASSADO ("back then",
// "as of March", "before I moved"). Quando anchor é detectado, search filtra
// entities/edges/chunks por valid_at / created_at <= anchor. Sem anchor,
// comportamento default permanece (estado current).
//
// Pra "currently" / "now" / "today" explícitos: também conta como CURRENT
// (= now), mas isso já é o default, então não retornamos anchor.
var (
	// Past-tense temporal anchors: "back then", "before X", "as of X", "in <year/month>"
	asOfBeforeRegex = regexp.MustCompile(`(?i)\b(before|prior to|antes de|antes do|antes da)\b`)
	asOfAsOfRegex   = regexp.MustCompile(`(?i)\bas of\b|\bnaquele\b|\bnaquela\b|\bem (janeiro|fevereiro|mar[çc]o|abril|maio|junho|julho|agosto|setembro|outubro|novembro|dezembro)\b`)
	asOfBackRegex   = regexp.MustCompile(`(?i)\b(back then|naquela época|naquele tempo|na época)\b`)
	asOfInYearRegex = regexp.MustCompile(`\b(in|em)\s+(19|20|21)(\d{2})\b`)

	// Reusa datas absolutas
	asOfISODate = regexp.MustCompile(`\b(\d{4})[-/](\d{1,2})[-/](\d{1,2})\b`)
	asOfEnMonth = regexp.MustCompile(`(?i)\b(january|february|march|april|may|june|july|august|september|october|november|december|jan|feb|mar|apr|jun|jul|aug|sep|sept|oct|nov|dec)\s+(\d{1,2})(?:[,\s]+(\d{4}))?\b`)
	asOfPtMonth = regexp.MustCompile(`(?i)\b(\d{1,2})\s+de\s+(janeiro|fevereiro|mar[çc]o|abril|maio|junho|julho|agosto|setembro|outubro|novembro|dezembro)(?:\s+de\s+(\d{4}))?\b`)
	asOfRelAgo  = regexp.MustCompile(`(?i)\b(\d+)\s+(day|days|week|weeks|month|months|year|years|dia|dias|semana|semanas|m[eê]s|meses|ano|anos)\s+(ago|atr[áa]s)\b`)
)

var asOfMonthEN = map[string]time.Month{
	"january": 1, "jan": 1, "february": 2, "feb": 2, "march": 3, "mar": 3,
	"april": 4, "apr": 4, "may": 5, "june": 6, "jun": 6, "july": 7, "jul": 7,
	"august": 8, "aug": 8, "september": 9, "sep": 9, "sept": 9,
	"october": 10, "oct": 10, "november": 11, "nov": 11, "december": 12, "dec": 12,
}

var asOfMonthPT = map[string]time.Month{
	"janeiro": 1, "fevereiro": 2, "março": 3, "marco": 3, "abril": 4,
	"maio": 5, "junho": 6, "julho": 7, "agosto": 8, "setembro": 9,
	"outubro": 10, "novembro": 11, "dezembro": 12,
}

// DetectQueryAsOf retorna o anchor temporal pra time-travel quando a query
// pede claramente um snapshot do passado. Retorna zero time se a query é
// "currently"-flavored (default current state) OU se não há sinal temporal.
//
// Sinais de PAST anchor:
//   - "back then", "naquela época", "na época"
//   - "before X" / "antes de X" + data parseável
//   - "as of <date>"
//   - "in <year>" / "em <ano>" (e.g. "in 2024")
//   - "in <month>" / "em <mês>" (e.g. "in March", "em março") — assume ano atual ou anterior
//   - "<N> days/months/years ago" → data resolvida
//   - Data ISO/Month-Day standalone num contexto past-flavored
func DetectQueryAsOf(question string, now time.Time) time.Time {
	hasPast := asOfBeforeRegex.MatchString(question) ||
		asOfAsOfRegex.MatchString(question) ||
		asOfBackRegex.MatchString(question) ||
		asOfInYearRegex.MatchString(question)

	// "<N> ago/atrás" sempre conta como past
	if m := asOfRelAgo.FindStringSubmatch(question); m != nil {
		n, _ := strconv.Atoi(m[1])
		unit := lowerAsciiSimple(m[2])
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
	}

	if !hasPast {
		return time.Time{}
	}

	// Tenta extrair data específica
	if m := asOfISODate.FindStringSubmatch(question); m != nil {
		y, _ := strconv.Atoi(m[1])
		mo, _ := strconv.Atoi(m[2])
		d, _ := strconv.Atoi(m[3])
		if t := safeAsOfDate(y, mo, d); !t.IsZero() {
			return t
		}
	}
	if m := asOfEnMonth.FindStringSubmatch(question); m != nil {
		mo, ok := asOfMonthEN[lowerAsciiSimple(m[1])]
		if ok {
			d, _ := strconv.Atoi(m[2])
			y := now.Year()
			if m[3] != "" {
				y, _ = strconv.Atoi(m[3])
			}
			if t := safeAsOfDate(y, int(mo), d); !t.IsZero() {
				return t
			}
		}
	}
	if m := asOfPtMonth.FindStringSubmatch(question); m != nil {
		mo, ok := asOfMonthPT[lowerAsciiSimple(m[2])]
		if ok {
			d, _ := strconv.Atoi(m[1])
			y := now.Year()
			if m[3] != "" {
				y, _ = strconv.Atoi(m[3])
			}
			if t := safeAsOfDate(y, int(mo), d); !t.IsZero() {
				return t
			}
		}
	}
	if m := asOfInYearRegex.FindStringSubmatch(question); m != nil {
		century, _ := strconv.Atoi(m[2])
		yy, _ := strconv.Atoi(m[3])
		y := century*100 + yy
		// "in 2024" → fim do ano = 2024-12-31 (snapshot pega TUDO daquele ano)
		return time.Date(y, 12, 31, 23, 59, 59, 0, time.UTC)
	}
	// "in March" / "em março" sem ano: usa ano anterior pra não pegar futuro
	// (assume usuário fala de algo já passado quando diz "em março" + tem
	// anchor signal). Heurística simples: se mês não chegou ainda este ano,
	// usa ano anterior; senão usa este ano.
	if m := asOfAsOfRegex.FindStringSubmatch(question); m != nil {
		// asOfAsOfRegex capture group 1 é o nome do mês PT quando o branch "em <mês>" bateu
		if len(m) >= 2 && m[1] != "" {
			if mo, ok := asOfMonthPT[lowerAsciiSimple(m[1])]; ok {
				y := now.Year()
				if int(mo) > int(now.Month()) {
					y-- // mês futuro neste ano → assume ano passado
				}
				return time.Date(y, mo, 1, 0, 0, 0, 0, time.UTC)
			}
		}
	}
	// hasPast=true mas sem data parseável → retorna now (snapshot atual,
	// não-óbvio mas é o melhor que dá sem mais info). Comportamento conservador:
	// melhor retornar zero pra não filtrar nada (e search degrada pra current).
	return time.Time{}
}

func safeAsOfDate(y, m, d int) time.Time {
	if y < 1900 || y > 2200 || m < 1 || m > 12 || d < 1 || d > 31 {
		return time.Time{}
	}
	t := time.Date(y, time.Month(m), d, 23, 59, 59, 0, time.UTC) // fim do dia → captura tudo daquele dia
	if t.Year() != y || t.Month() != time.Month(m) || t.Day() != d {
		return time.Time{}
	}
	return t
}

func lowerAsciiSimple(s string) string {
	b := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 32
		}
		b[i] = c
	}
	return string(b)
}
