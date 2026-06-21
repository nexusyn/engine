package query

import "testing"

func TestIsCountQuery(t *testing.T) {
	cases := []struct {
		q    string
		want bool
	}{
		{"How many plants did I acquire?", true},
		{"How much did I spend on furniture?", true},
		{"What is the total number of trips?", true},
		{"How often do I go to the gym?", true},
		{"Quantos livros eu li?", true},
		{"Quantas vezes fui ao dentista?", true},
		{"Número de viagens que fiz?", true},
		{"Total de gastos no mês?", true},
		{"What does Luciano drink?", false},
		{"Where do I live?", false},
		{"Recommend a podcast", false},
		{"When did I start learning Go?", false},
	}
	for _, c := range cases {
		t.Run(c.q, func(t *testing.T) {
			if got := IsCountQuery(c.q); got != c.want {
				t.Errorf("IsCountQuery(%q) = %v, want %v", c.q, got, c.want)
			}
		})
	}
}

func TestEffectiveLimit(t *testing.T) {
	// count query abaixo do recall limit → sobe pra RecallLimit
	if got := EffectiveLimit("How many plants?", 5); got != RecallLimit {
		t.Errorf("count query limit 5 → %d, want %d", got, RecallLimit)
	}
	// count query já acima do recall limit → mantém
	if got := EffectiveLimit("How many plants?", 50); got != 50 {
		t.Errorf("count query limit 50 → %d, want 50", got)
	}
	// non-count query → mantém o pedido
	if got := EffectiveLimit("Where do I live?", 5); got != 5 {
		t.Errorf("non-count query limit 5 → %d, want 5", got)
	}
	if got := EffectiveLimit("Where do I live?", 10); got != 10 {
		t.Errorf("non-count query limit 10 → %d, want 10", got)
	}
}
