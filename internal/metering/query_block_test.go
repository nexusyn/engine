package metering

import (
	"testing"
	"time"
)

func TestQueryBlockActive(t *testing.T) {
	before := time.Date(2026, 9, 30, 23, 59, 0, 0, saoPaulo())
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, saoPaulo())

	prev := enforced.Load()
	enforced.Store(false)
	t.Cleanup(func() { enforced.Store(prev) })

	if QueryBlockActive(before) {
		t.Fatal("antes de 1/out/2026, com enforce off, query não bloqueia")
	}
	if !QueryBlockActive(start) {
		t.Fatal("a partir de 1/out/2026 a query acima da cota bloqueia")
	}

	since := time.Date(2026, 10, 1, 9, 0, 0, 0, saoPaulo())
	if GraceElapsed(since.Add(71*time.Hour), since) {
		t.Fatal("dentro de 72h a query ainda passa")
	}
	if !GraceElapsed(since.Add(72*time.Hour), since) {
		t.Fatal("às 72h o corte vence")
	}
	if GraceElapsed(since, time.Time{}) {
		t.Fatal("sem relógio armado não bloqueia")
	}
}
