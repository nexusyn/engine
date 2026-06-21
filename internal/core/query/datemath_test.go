package query

import (
	"strings"
	"testing"
	"time"
)

func TestIsDateMathQuery(t *testing.T) {
	cases := []struct {
		q    string
		want bool
	}{
		{"How many days between March 15 and March 27?", true},
		{"Quantos dias entre 2026-03-15 e 2026-03-27?", true},
		{"How long since I visited Nordstrom?", true},
		{"How many weeks ago was the sale?", true},
		{"Há quanto tempo desde 10 de março?", true},
		{"5 days ago I went to the gym, when was that?", true},
		{"What does Luciano drink?", false},
		{"Recommend a podcast", false},
		{"List the museums I visited", false},
	}
	for _, c := range cases {
		t.Run(c.q, func(t *testing.T) {
			if got := IsDateMathQuery(c.q); got != c.want {
				t.Errorf("IsDateMathQuery(%q) = %v, want %v", c.q, got, c.want)
			}
		})
	}
}

func TestComputeDateMath_Between(t *testing.T) {
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	facts := ComputeDateMath("How many days between 2026-03-15 and 2026-03-27?", now)
	if len(facts) != 1 {
		t.Fatalf("expected 1 fact, got %d: %+v", len(facts), facts)
	}
	if !strings.Contains(facts[0].Description, "12 days") {
		t.Errorf("expected '12 days' in description, got: %s", facts[0].Description)
	}
	if !strings.Contains(facts[0].Description, "2026-03-15") {
		t.Errorf("expected source date in description, got: %s", facts[0].Description)
	}
}

func TestComputeDateMath_BetweenEnMonthDay(t *testing.T) {
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	facts := ComputeDateMath("Days between March 15 and March 27?", now)
	if len(facts) != 1 {
		t.Fatalf("expected 1 fact, got %d: %+v", len(facts), facts)
	}
	if !strings.Contains(facts[0].Description, "12 days") {
		t.Errorf("expected '12 days', got: %s", facts[0].Description)
	}
}

func TestComputeDateMath_BetweenPtMonthDay(t *testing.T) {
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	facts := ComputeDateMath("Quantos dias entre 15 de março e 27 de março?", now)
	if len(facts) != 1 {
		t.Fatalf("expected 1 fact, got %d: %+v", len(facts), facts)
	}
	if !strings.Contains(facts[0].Description, "12 days") {
		t.Errorf("expected '12 days', got: %s", facts[0].Description)
	}
}

func TestComputeDateMath_Since(t *testing.T) {
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	facts := ComputeDateMath("How many days since 2026-05-09?", now)
	if len(facts) != 1 {
		t.Fatalf("expected 1 fact, got %d", len(facts))
	}
	if !strings.Contains(facts[0].Description, "14 days") {
		t.Errorf("expected '14 days', got: %s", facts[0].Description)
	}
	if !strings.Contains(facts[0].Description, "today: 2026-05-23") {
		t.Errorf("expected today marker, got: %s", facts[0].Description)
	}
}

func TestComputeDateMath_NDaysAgo(t *testing.T) {
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	// "5 days ago" → data resolvida = 2026-05-18 → diff vs today = 5 days
	facts := ComputeDateMath("What did I do 5 days ago?", now)
	if len(facts) != 1 {
		t.Fatalf("expected 1 fact, got %d: %+v", len(facts), facts)
	}
	if !strings.Contains(facts[0].Description, "5 days") {
		t.Errorf("expected '5 days', got: %s", facts[0].Description)
	}
	if !strings.Contains(facts[0].Description, "2026-05-18") {
		t.Errorf("expected resolved date 2026-05-18, got: %s", facts[0].Description)
	}
}

func TestComputeDateMath_WeeksUnit(t *testing.T) {
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	facts := ComputeDateMath("How many weeks since 2026-04-25?", now)
	if len(facts) != 1 {
		t.Fatalf("expected 1 fact, got %d", len(facts))
	}
	// 28 days = 4 weeks → entra no branch >=14 && <60
	if !strings.Contains(facts[0].Description, "weeks") {
		t.Errorf("expected weeks notation, got: %s", facts[0].Description)
	}
}

func TestComputeDateMath_NoIntent(t *testing.T) {
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	if facts := ComputeDateMath("What does Luciano drink?", now); facts != nil {
		t.Errorf("expected nil for non date-math query, got: %+v", facts)
	}
}

func TestComputeDateMath_IntentNoDates(t *testing.T) {
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	// Intent presente mas sem datas extraíveis → no-op
	if facts := ComputeDateMath("How long has it been since we last spoke?", now); facts != nil {
		t.Errorf("expected nil when no dates extractable, got: %+v", facts)
	}
}

func TestComputeDateMath_OrderIndependent(t *testing.T) {
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	// Datas em ordem reversa → formatBetween normaliza
	facts := ComputeDateMath("Days between 2026-03-27 and 2026-03-15?", now)
	if len(facts) != 1 {
		t.Fatalf("expected 1 fact, got %d", len(facts))
	}
	if !strings.Contains(facts[0].Description, "12 days") {
		t.Errorf("expected '12 days' regardless of order, got: %s", facts[0].Description)
	}
}

func TestFormatDateMathFacts(t *testing.T) {
	got := FormatDateMathFacts(nil)
	if got != "" {
		t.Errorf("expected empty string for nil, got: %q", got)
	}
	got = FormatDateMathFacts([]DateMathFact{
		{Description: "12 days between A and B"},
		{Description: "5 days since X"},
	})
	if !strings.Contains(got, "COMPUTED FACTS") {
		t.Errorf("expected header in output, got: %q", got)
	}
	if !strings.Contains(got, "- 12 days between A and B") {
		t.Errorf("expected first fact rendered, got: %q", got)
	}
	if !strings.Contains(got, "- 5 days since X") {
		t.Errorf("expected second fact rendered, got: %q", got)
	}
}

func TestExtractDates_InvalidIgnored(t *testing.T) {
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	// 2026-13-45 é inválido (mês 13, dia 45) — não deve entrar
	dates := extractDates("between 2026-13-45 and 2026-05-10", now)
	for _, d := range dates {
		if d.Month() == 13 {
			t.Errorf("invalid date leaked: %v", d)
		}
	}
}

func TestMonthsBetween(t *testing.T) {
	a := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	b := time.Date(2026, 5, 14, 0, 0, 0, 0, time.UTC)
	// Jan 15 → May 14: 3 meses completos (não 4, porque day 14 < day 15)
	if got := monthsBetween(a, b); got != 3 {
		t.Errorf("monthsBetween(Jan15, May14) = %d, want 3", got)
	}
	// Inverte ordem
	if got := monthsBetween(b, a); got != 3 {
		t.Errorf("monthsBetween reversed = %d, want 3", got)
	}
}
