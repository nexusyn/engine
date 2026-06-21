package dateutil

import (
	"reflect"
	"testing"
)

func TestCanonicalDates(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"reunião em 2026.06.01 foi ruim", []string{"2026-06-01"}},
		{"2026-06-01 e 2026/06/02", []string{"2026-06-01", "2026-06-02"}},
		{"foi dia 01/06/2026", []string{"2026-06-01"}},
		{"1 de junho de 2026 e 2 de junho de 2026", []string{"2026-06-01", "2026-06-02"}},
		{"on June 1, 2026 it happened", []string{"2026-06-01"}},
		{"1 June 2026 was the day", []string{"2026-06-01"}},
		{"março 5 2026 e 05/03/2026 (mesma data)", []string{"2026-03-05"}},
		{"sem data aqui", nil},
		{"só dia 5 de junho sem ano", nil}, // sem ano → ignora
		{"data inválida 2026-13-40", nil},
	}
	for _, c := range cases {
		got := CanonicalDates(c.in)
		if len(got) == 0 && len(c.want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("CanonicalDates(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestStripContextDate(t *testing.T) {
	// o [Today:...] não deve gerar token de data (canal dispara só na data perguntada)
	q := "[Today: 2026/06/01] o que aconteceu em 2 de junho de 2026?"
	got := DateSearchTokens(StripContextDate(q))
	if got != "d20260602" {
		t.Errorf("StripContextDate: got %q, want d20260602 (sem o today)", got)
	}
	if StripContextDate("sem marcador 2026-06-02") != "sem marcador 2026-06-02" {
		t.Error("StripContextDate alterou texto sem marcador")
	}
}
