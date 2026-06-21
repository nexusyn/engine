package search

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// dateAnchorRegex reusa o mesmo padrão do package query/datemath: ISO
// + "Month DD[, YYYY]" + "DD de mês [de YYYY]". Mantido localmente pra evitar
// dep cíclica (search → query → search via integration tests).
var (
	dateAnchorISO    = regexp.MustCompile(`\b(\d{4})[-/](\d{1,2})[-/](\d{1,2})\b`)
	dateAnchorEnMD   = regexp.MustCompile(`(?i)\b(january|february|march|april|may|june|july|august|september|october|november|december|jan|feb|mar|apr|jun|jul|aug|sep|sept|oct|nov|dec)\s+(\d{1,2})(?:[,\s]+(\d{4}))?\b`)
	dateAnchorPtDdM  = regexp.MustCompile(`(?i)\b(\d{1,2})\s+de\s+(janeiro|fevereiro|mar[çc]o|abril|maio|junho|julho|agosto|setembro|outubro|novembro|dezembro)(?:\s+de\s+(\d{4}))?\b`)
	dateAnchorRelAgo = regexp.MustCompile(`(?i)\b(\d+)\s+(day|days|week|weeks|month|months|dia|dias|semana|semanas|m[eê]s|meses)\s+(ago|atr[áa]s)\b`)
)

var anchorMonthEN = map[string]time.Month{
	"january": 1, "jan": 1, "february": 2, "feb": 2, "march": 3, "mar": 3,
	"april": 4, "apr": 4, "may": 5, "june": 6, "jun": 6, "july": 7, "jul": 7,
	"august": 8, "aug": 8, "september": 9, "sep": 9, "sept": 9,
	"october": 10, "oct": 10, "november": 11, "nov": 11, "december": 12, "dec": 12,
}

var anchorMonthPT = map[string]time.Month{
	"janeiro": 1, "fevereiro": 2, "março": 3, "marco": 3, "abril": 4,
	"maio": 5, "junho": 6, "julho": 7, "agosto": 8, "setembro": 9,
	"outubro": 10, "novembro": 11, "dezembro": 12,
}

// DetectDateAnchor retorna a primeira data plausivelmente mencionada na query
// (zero time se nenhuma). Pra date-anchored retrieval boost: se o usuário
// menciona "March 15", chunks com session_date próxima ganham score extra.
func DetectDateAnchor(question string, now time.Time) time.Time {
	if m := dateAnchorISO.FindStringSubmatch(question); m != nil {
		y, _ := strconv.Atoi(m[1])
		mo, _ := strconv.Atoi(m[2])
		d, _ := strconv.Atoi(m[3])
		if t := safeDate(y, mo, d); !t.IsZero() {
			return t
		}
	}
	if m := dateAnchorEnMD.FindStringSubmatch(question); m != nil {
		mo, ok := anchorMonthEN[lowerASCII(m[1])]
		if ok {
			d, _ := strconv.Atoi(m[2])
			y := now.Year()
			if m[3] != "" {
				y, _ = strconv.Atoi(m[3])
			}
			if t := safeDate(y, int(mo), d); !t.IsZero() {
				return t
			}
		}
	}
	if m := dateAnchorPtDdM.FindStringSubmatch(question); m != nil {
		mo, ok := anchorMonthPT[lowerASCII(m[2])]
		if ok {
			d, _ := strconv.Atoi(m[1])
			y := now.Year()
			if m[3] != "" {
				y, _ = strconv.Atoi(m[3])
			}
			if t := safeDate(y, int(mo), d); !t.IsZero() {
				return t
			}
		}
	}
	if m := dateAnchorRelAgo.FindStringSubmatch(question); m != nil {
		n, _ := strconv.Atoi(m[1])
		unit := lowerASCII(m[2])
		switch unit {
		case "day", "days", "dia", "dias":
			return now.AddDate(0, 0, -n)
		case "week", "weeks", "semana", "semanas":
			return now.AddDate(0, 0, -7*n)
		case "month", "months", "mês", "mes", "meses":
			return now.AddDate(0, -n, 0)
		}
	}
	return time.Time{}
}

func safeDate(y, mo, d int) time.Time {
	if y < 1900 || y > 2200 || mo < 1 || mo > 12 || d < 1 || d > 31 {
		return time.Time{}
	}
	t := time.Date(y, time.Month(mo), d, 0, 0, 0, 0, time.UTC)
	if t.Year() != y || t.Month() != time.Month(mo) || t.Day() != d {
		return time.Time{}
	}
	return t
}

func lowerASCII(s string) string {
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

// loadChunkSessionDates extrai [Session date: YYYY/MM/DD] do content via regex
// SQL. Zero schema change (vs. adicionar coluna chunks.session_date). Custo:
// regex em N candidatos por query (~50-100 chunks) — Postgres trata bem.
//
// Retorna map[chunk_id]session_date. Chunks sem header retornam zero time
// no map (caller pula o boost pra eles).
func loadChunkSessionDates(ctx context.Context, tx pgx.Tx, ids []int64) (map[int64]time.Time, error) {
	if len(ids) == 0 {
		return map[int64]time.Time{}, nil
	}
	// substring com regex Postgres POSIX. Suporta YYYY/MM/DD e YYYY-MM-DD.
	// Captura grupo 1 (a data inteira).
	rows, err := tx.Query(ctx, `
		SELECT id, substring(content from '\[(?:Session date|Date|Today)\s*:\s*([0-9]{4}[-/][0-9]{1,2}[-/][0-9]{1,2})\s*\]') AS sess_date
		FROM chunks
		WHERE id = ANY($1)
	`, ids)
	if err != nil {
		return nil, fmt.Errorf("load session dates: %w", err)
	}
	defer rows.Close()

	out := make(map[int64]time.Time, len(ids))
	for rows.Next() {
		var id int64
		var dateStr *string
		if err := rows.Scan(&id, &dateStr); err != nil {
			return nil, fmt.Errorf("session date scan: %w", err)
		}
		if dateStr == nil || *dateStr == "" {
			continue
		}
		if t := parseSessionDate(*dateStr); !t.IsZero() {
			out[id] = t
		}
	}
	return out, rows.Err()
}

func parseSessionDate(s string) time.Time {
	for _, layout := range []string{"2006/1/2", "2006-1-2", "2006/01/02", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// ApplyDateAnchorBoost re-scoreia chunks por proximidade ao anchor date.
//
// new_score = old_score * (1 + maxBoost * exp(-diff_days / falloff))
//
// Onde diff_days é |chunk.session_date - anchor| em dias. Falloff controla
// quão rapidamente o boost cai com distância (default 7d — chunks dentro de
// 1 semana do anchor ganham forte boost; >30d ganham quase nada).
//
// maxBoost = 1.0 → chunk no anchor exato dobra de score. 0 desativa.
// Chunks sem session_date no map mantêm score original.
func ApplyDateAnchorBoost(scores map[int64]float64, sessionDates map[int64]time.Time, anchor time.Time, maxBoost float64, falloffDays float64) map[int64]float64 {
	if anchor.IsZero() || maxBoost <= 0 || falloffDays <= 0 {
		return scores
	}
	out := make(map[int64]float64, len(scores))
	for id, score := range scores {
		t, ok := sessionDates[id]
		if !ok || t.IsZero() {
			out[id] = score
			continue
		}
		diff := math.Abs(anchor.Sub(t).Hours() / 24.0)
		multiplier := 1.0 + maxBoost*math.Exp(-diff/falloffDays)
		out[id] = score * multiplier
	}
	return out
}
