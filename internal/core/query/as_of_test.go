package query

import (
	"testing"
	"time"
)

func TestDetectQueryAsOf_NoSignal(t *testing.T) {
	now := time.Date(2026, 5, 23, 0, 0, 0, 0, time.UTC)
	if got := DetectQueryAsOf("What does Luciano drink?", now); !got.IsZero() {
		t.Errorf("plain query should return zero, got %v", got)
	}
}

func TestDetectQueryAsOf_RelativeAgo(t *testing.T) {
	now := time.Date(2026, 5, 23, 0, 0, 0, 0, time.UTC)
	got := DetectQueryAsOf("Where did I live 2 years ago?", now)
	want := time.Date(2024, 5, 23, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestDetectQueryAsOf_AtrasPortugues(t *testing.T) {
	now := time.Date(2026, 5, 23, 0, 0, 0, 0, time.UTC)
	got := DetectQueryAsOf("Onde eu morava 3 meses atrás?", now)
	want := time.Date(2026, 2, 23, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestDetectQueryAsOf_BeforeWithDate(t *testing.T) {
	now := time.Date(2026, 5, 23, 0, 0, 0, 0, time.UTC)
	got := DetectQueryAsOf("What was my address before 2025-08-15?", now)
	want := time.Date(2025, 8, 15, 23, 59, 59, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestDetectQueryAsOf_AsOfPlusDate(t *testing.T) {
	now := time.Date(2026, 5, 23, 0, 0, 0, 0, time.UTC)
	got := DetectQueryAsOf("As of March 1 2025 where was I?", now)
	want := time.Date(2025, 3, 1, 23, 59, 59, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestDetectQueryAsOf_BackThen(t *testing.T) {
	now := time.Date(2026, 5, 23, 0, 0, 0, 0, time.UTC)
	// "back then" sem data → conservador, retorna zero
	if got := DetectQueryAsOf("What was I doing back then?", now); !got.IsZero() {
		t.Errorf("'back then' without specific date should be zero (conservative), got %v", got)
	}
}

func TestDetectQueryAsOf_InYear(t *testing.T) {
	now := time.Date(2026, 5, 23, 0, 0, 0, 0, time.UTC)
	got := DetectQueryAsOf("What did I work on in 2024?", now)
	want := time.Date(2024, 12, 31, 23, 59, 59, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestDetectQueryAsOf_CurrentlyShouldNotSetAnchor(t *testing.T) {
	now := time.Date(2026, 5, 23, 0, 0, 0, 0, time.UTC)
	// "currently" / "now" / "today" = default state, no anchor needed
	if got := DetectQueryAsOf("Where do I currently live?", now); !got.IsZero() {
		t.Errorf("'currently' should not set anchor (default = current), got %v", got)
	}
	if got := DetectQueryAsOf("What am I doing now?", now); !got.IsZero() {
		t.Errorf("'now' should not set anchor, got %v", got)
	}
}

func TestDetectQueryAsOf_InvalidDateIgnored(t *testing.T) {
	now := time.Date(2026, 5, 23, 0, 0, 0, 0, time.UTC)
	// "before 2026-13-45" — data inválida, ainda tem "before" mas data não parse
	got := DetectQueryAsOf("What was happening before 2026-13-45?", now)
	if !got.IsZero() {
		t.Errorf("invalid date with anchor word should fallback to zero, got %v", got)
	}
}
