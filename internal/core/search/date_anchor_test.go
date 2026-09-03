package search

import (
	"testing"
	"time"
)

func TestDetectDateAnchor_ISO(t *testing.T) {
	now := time.Date(2026, 5, 23, 0, 0, 0, 0, time.UTC)
	got := DetectDateAnchor("What happened on 2026-03-15?", now)
	want := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestDetectDateAnchor_EnMonthDay(t *testing.T) {
	now := time.Date(2026, 5, 23, 0, 0, 0, 0, time.UTC)
	got := DetectDateAnchor("What did I do on March 15?", now)
	want := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestDetectDateAnchor_EnMonthDayYear(t *testing.T) {
	now := time.Date(2026, 5, 23, 0, 0, 0, 0, time.UTC)
	got := DetectDateAnchor("Anything from August 2 2025?", now)
	want := time.Date(2025, 8, 2, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestDetectDateAnchor_PtDiaDeMes(t *testing.T) {
	now := time.Date(2026, 5, 23, 0, 0, 0, 0, time.UTC)
	got := DetectDateAnchor("O que aconteceu em 15 de março?", now)
	want := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestDetectDateAnchor_RelativeAgo(t *testing.T) {
	now := time.Date(2026, 5, 23, 0, 0, 0, 0, time.UTC)
	got := DetectDateAnchor("What did I do 5 days ago?", now)
	want := time.Date(2026, 5, 18, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestDetectDateAnchor_WordNumberAgo(t *testing.T) {
	now := time.Date(2026, 5, 23, 0, 0, 0, 0, time.UTC)
	// "four weeks ago" = -28 dias (antes virava âncora vazia/hoje)
	got := DetectDateAnchor("the business milestone I mentioned four weeks ago", now)
	want := time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestDetectDateAnchor_ArticleWeekAgo(t *testing.T) {
	now := time.Date(2026, 5, 23, 0, 0, 0, 0, time.UTC)
	got := DetectDateAnchor("Which book did I finish a week ago?", now)
	want := time.Date(2026, 5, 16, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestDetectDateAnchor_LastWeek(t *testing.T) {
	now := time.Date(2026, 5, 23, 0, 0, 0, 0, time.UTC)
	got := DetectDateAnchor("What did I buy last week?", now)
	want := time.Date(2026, 5, 16, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestDetectDateAnchor_LastWeekdayEN(t *testing.T) {
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	got := DetectDateAnchor("What artist did I start listening to last Friday?", now)
	if got.IsZero() {
		t.Fatal("esperava uma data para 'last Friday'")
	}
	if got.Weekday() != time.Friday {
		t.Errorf("esperava sexta-feira, got %v (%v)", got.Weekday(), got)
	}
	if !got.Before(now) || now.Sub(got) > 8*24*time.Hour {
		t.Errorf("esperava nos últimos 7 dias, got %v", got)
	}
}

func TestDetectDateAnchor_PtWeekdayPassada(t *testing.T) {
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	got := DetectDateAnchor("o que eu fiz na sexta passada?", now)
	if got.IsZero() || got.Weekday() != time.Friday {
		t.Errorf("esperava uma sexta-feira passada, got %v", got)
	}
}

func TestDetectDateAnchor_NoMatch(t *testing.T) {
	now := time.Date(2026, 5, 23, 0, 0, 0, 0, time.UTC)
	if got := DetectDateAnchor("What does Luciano drink?", now); !got.IsZero() {
		t.Errorf("expected zero time for no-date query, got %v", got)
	}
}

func TestDetectDateAnchor_InvalidIgnored(t *testing.T) {
	now := time.Date(2026, 5, 23, 0, 0, 0, 0, time.UTC)
	// Month 13 inválido
	if got := DetectDateAnchor("What about 2026-13-45?", now); !got.IsZero() {
		t.Errorf("invalid date should give zero, got %v", got)
	}
}

func TestParseSessionDate_FormatVariants(t *testing.T) {
	cases := map[string]time.Time{
		"2026/03/15": time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC),
		"2026-03-15": time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC),
		"2026/3/5":   time.Date(2026, 3, 5, 0, 0, 0, 0, time.UTC),
	}
	for in, want := range cases {
		if got := parseSessionDate(in); !got.Equal(want) {
			t.Errorf("parseSessionDate(%q) = %v, want %v", in, got, want)
		}
	}
	if got := parseSessionDate("not a date"); !got.IsZero() {
		t.Errorf("parseSessionDate('not a date') should be zero, got %v", got)
	}
}

func TestApplyDateAnchorBoost_NoOpZeroAnchor(t *testing.T) {
	scores := map[int64]float64{1: 0.5, 2: 0.3}
	sd := map[int64]time.Time{1: time.Now(), 2: time.Now()}
	got := ApplyDateAnchorBoost(scores, sd, time.Time{}, 1.0, 7)
	for id, v := range got {
		if v != scores[id] {
			t.Errorf("zero anchor should be no-op, id %d: got %f, want %f", id, v, scores[id])
		}
	}
}

func TestApplyDateAnchorBoost_BoostsNearAnchor(t *testing.T) {
	anchor := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)
	scores := map[int64]float64{
		1: 0.5, // chunk exatamente no anchor
		2: 0.5, // 30 dias de distância
		3: 0.5, // sem session_date
	}
	sd := map[int64]time.Time{
		1: anchor,
		2: anchor.AddDate(0, 0, 30),
		// 3 ausente intencionalmente
	}
	got := ApplyDateAnchorBoost(scores, sd, anchor, 1.0, 7.0)
	if got[1] <= scores[1] {
		t.Errorf("chunk at anchor should be boosted: got %f, was %f", got[1], scores[1])
	}
	if got[1] != 1.0 { // 0.5 * (1 + 1.0 * exp(0)) = 0.5 * 2 = 1.0
		t.Errorf("chunk at anchor: got %f, want 1.0 (0.5 * 2)", got[1])
	}
	if got[2] >= got[1] {
		t.Errorf("chunk far from anchor should be < chunk at anchor: got %f vs %f", got[2], got[1])
	}
	if got[3] != scores[3] {
		t.Errorf("chunk without session_date should be unchanged: got %f, want %f", got[3], scores[3])
	}
}

func TestApplyDateAnchorBoost_FarChunkBarelyBoosted(t *testing.T) {
	anchor := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)
	scores := map[int64]float64{1: 1.0}
	sd := map[int64]time.Time{1: anchor.AddDate(0, 0, 60)}
	got := ApplyDateAnchorBoost(scores, sd, anchor, 1.0, 7.0)
	// 60 dias / 7 falloff → exp(-8.57) ≈ 1.9e-4 → multiplier ≈ 1.00019
	if got[1] >= 1.01 {
		t.Errorf("chunk 60 days away should be ~unchanged, got %f", got[1])
	}
	if got[1] < 1.0 {
		t.Errorf("boost should not decrease score, got %f", got[1])
	}
}

func TestApplyDateAnchorBoost_WeakOnAnchorBeatsStrongFar(t *testing.T) {
	// Caso temporal: a resposta certa está NA data da âncora mas é semanticamente
	// fraca (RRF baixo); um chunk irrelevante porém forte está 30 dias longe.
	// Com maxBoost=4.0 o chunk da data exata DEVE superá-lo — com o antigo 1.0 NÃO
	// superava (é exatamente a razão de subir o boost: recall temporal).
	anchor := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)
	scores := map[int64]float64{1: 0.10, 2: 0.25}
	sd := map[int64]time.Time{1: anchor, 2: anchor.AddDate(0, 0, -30)}
	got := ApplyDateAnchorBoost(scores, sd, anchor, 4.0, 7.0)
	if got[1] <= got[2] {
		t.Errorf("fraco-na-âncora (%.3f) deveria superar forte-distante (%.3f)", got[1], got[2])
	}
	old := ApplyDateAnchorBoost(scores, sd, anchor, 1.0, 7.0)
	if old[1] > old[2] {
		t.Errorf("com maxBoost=1.0 o fraco NÃO deveria superar — teste perderia o sentido")
	}
}
