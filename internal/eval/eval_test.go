package eval

import "testing"

func TestCases_EmbeddedSet(t *testing.T) {
	cs, err := Cases()
	if err != nil {
		t.Fatalf("Cases() erro: %v", err)
	}
	if len(cs) != 12 {
		t.Errorf("len(cases) = %d, quer 12", len(cs))
	}
	types := map[string]int{}
	for _, c := range cs {
		if c.ID == "" || c.Type == "" || c.Question == "" || c.Context == "" || c.Gold == "" {
			t.Errorf("caso %q tem campo vazio: %+v", c.ID, c)
		}
		types[c.Type]++
	}
	if len(types) != 6 {
		t.Errorf("categorias = %d (%v), quer 6", len(types), types)
	}
	for typ, n := range types {
		if n != 2 {
			t.Errorf("categoria %q tem %d casos, quer 2", typ, n)
		}
	}
}
