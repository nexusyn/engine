package profile

import (
	"strings"
	"testing"
)

func TestFormatProfileBlock_Empty(t *testing.T) {
	if got := FormatProfileBlock(Profile{}); got != "" {
		t.Errorf("expected empty for zero Profile, got %q", got)
	}
	if got := FormatProfileBlock(Profile{Content: "   \n  \t  "}); got != "" {
		t.Errorf("expected empty for whitespace-only content, got %q", got)
	}
}

func TestFormatProfileBlock_WithContent(t *testing.T) {
	p := Profile{Content: "## Likes\n- espresso\n- Ted Chiang"}
	got := FormatProfileBlock(p)
	if !strings.Contains(got, "USER PROFILE") {
		t.Errorf("expected header in output, got %q", got)
	}
	if !strings.Contains(got, "## Likes") {
		t.Errorf("expected content in output, got %q", got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("expected trailing newline, got %q", got)
	}
}

func TestFormatProfileBlock_ContentAlreadyEndsWithNewline(t *testing.T) {
	p := Profile{Content: "## Likes\n- espresso\n"}
	got := FormatProfileBlock(p)
	// Não deve duplicar newline
	if strings.HasSuffix(got, "\n\n\n") {
		t.Errorf("expected exactly one trailing newline section, got %q", got)
	}
}

func TestCountByKind(t *testing.T) {
	facts := []rawFact{
		{Kind: "preference"},
		{Kind: "preference"},
		{Kind: "lesson"},
		{Kind: "preference"},
	}
	prefs, lessons := countByKind(facts)
	if prefs != 3 {
		t.Errorf("preferences count = %d, want 3", prefs)
	}
	if lessons != 1 {
		t.Errorf("lessons count = %d, want 1", lessons)
	}
}

func TestCountByKind_Empty(t *testing.T) {
	prefs, lessons := countByKind(nil)
	if prefs != 0 || lessons != 0 {
		t.Errorf("empty input should give (0,0), got (%d,%d)", prefs, lessons)
	}
}

func TestRenderFactsForLLM(t *testing.T) {
	facts := []rawFact{
		{Name: "espresso", Kind: "preference", Polarity: "like", Category: "drink"},
		{Name: "morning meetings", Kind: "preference", Polarity: "dislike", Detail: "before 10am"},
		{Name: "always profile before optimizing", Kind: "lesson"},
	}
	got := renderFactsForLLM(facts)
	if !strings.Contains(got, "[preference] LIKE: espresso") {
		t.Errorf("expected like fact rendered with polarity, got %q", got)
	}
	if !strings.Contains(got, "category=drink") {
		t.Errorf("expected category extra, got %q", got)
	}
	if !strings.Contains(got, "DISLIKE: morning meetings (before 10am)") {
		t.Errorf("expected dislike fact with detail, got %q", got)
	}
	if !strings.Contains(got, "[lesson] always profile") {
		t.Errorf("expected lesson rendered, got %q", got)
	}
}

func TestRenderFactsForLLM_NeutralPolarityDropped(t *testing.T) {
	facts := []rawFact{
		{Name: "vim", Kind: "preference", Polarity: "neutral"},
	}
	got := renderFactsForLLM(facts)
	if strings.Contains(got, "NEUTRAL") {
		t.Errorf("neutral polarity should not be rendered as prefix, got %q", got)
	}
}
