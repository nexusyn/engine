package api

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/nexusyn/engine/internal/core/guideline"
	"github.com/nexusyn/engine/internal/core/profile"
)

func TestCtxClampMaxTokens(t *testing.T) {
	cases := []struct {
		raw  string
		want int
	}{
		{"", ctxDefaultMaxTokens},
		{"abc", ctxDefaultMaxTokens},
		{"0", ctxDefaultMaxTokens},
		{"-5", ctxDefaultMaxTokens},
		{"50", ctxMinMaxTokens},     // abaixo do mínimo → clamp p/ min
		{"999999", ctxMaxMaxTokens}, // acima do máximo → clamp p/ max
		{"1200", 1200},              // dentro da faixa → passa
		{"  800  ", 800},            // trim
	}
	for _, c := range cases {
		if got := ctxClampMaxTokens(c.raw); got != c.want {
			t.Errorf("ctxClampMaxTokens(%q) = %d, want %d", c.raw, got, c.want)
		}
	}
}

func TestCapRunes(t *testing.T) {
	// vazio → não trunca
	if s, tr := capRunes("   ", 10); s != "" || tr {
		t.Errorf("capRunes(blank) = (%q, %v), want (\"\", false)", s, tr)
	}
	// cabe → inalterado
	if s, tr := capRunes("ação", 10); s != "ação" || tr {
		t.Errorf("capRunes fit = (%q, %v), want (ação, false)", s, tr)
	}
	// max<=0 com conteúdo → trunca tudo
	if s, tr := capRunes("x", 0); s != "" || !tr {
		t.Errorf("capRunes(max=0) = (%q, %v), want (\"\", true)", s, tr)
	}
	// estoura → trunca rune-safe (sem partir multibyte) e marca truncated
	in := strings.Repeat("ç", 20) // 20 runas, 40 bytes
	out, tr := capRunes(in, 5)
	if !tr {
		t.Fatalf("capRunes overflow: truncated=false, want true")
	}
	if !utf8.ValidString(out) {
		t.Errorf("capRunes produziu UTF-8 inválido: %q", out)
	}
	// o corpo (antes do marcador) deve ter no máx 5 runas de 'ç'
	body := strings.SplitN(out, "\n", 2)[0]
	if utf8.RuneCountInString(body) > 5 {
		t.Errorf("capRunes não respeitou o cap: corpo=%q (%d runas)", body, utf8.RuneCountInString(body))
	}
}

func TestRenderContextPack_Budget(t *testing.T) {
	guidelines := []guideline.Item{
		{ID: 1, Title: "Regra A", Content: strings.Repeat("alpha ", 500)},
		{ID: 2, Title: "Regra B", Content: strings.Repeat("beta ", 500)},
	}
	prof := profile.Profile{Content: strings.Repeat("perfil ", 200)}
	wiki := []contextWikiPage{
		{ID: 10, Title: "Pág 1", Preview: "preview\ncom\nquebras"},
		{ID: 11, Title: "Pág 2", Preview: strings.Repeat("z ", 200)},
	}
	recent := []contextRecentItem{
		{ID: 20, Title: "Mudança recente", Domain: "memory", CreatedAt: "2026-06-30"},
		{ID: 21, Title: strings.Repeat("x ", 200), Domain: "wiki", CreatedAt: "2026-06-29"},
	}

	maxTokens := 300
	maxRunes := maxTokens * ctxCharsPerToken
	allOn := map[string]bool{"guidelines": true, "profile": true, "index": true, "recent": true}
	pack, truncated := renderContextPack(7, "nexusyn", guidelines, prof, wiki, recent, allOn, maxRunes)

	if !truncated {
		t.Errorf("esperava truncated=true com budget apertado")
	}
	// O pacote total não pode exceder muito o budget: header + 4 seções capadas
	// a 50/15/20/15%. Margem p/ header + marcadores.
	got := utf8.RuneCountInString(pack)
	ceiling := maxRunes + 200
	if got > ceiling {
		t.Errorf("pacote = %d runas, acima do teto %d (budget %d)", got, ceiling, maxRunes)
	}
	// Deve conter as 4 seções e o header da org/projeto.
	for _, want := range []string{"org 7", "projeto nexusyn", "Guidelines da org", "Perfil do usuário", "Índice do projeto: nexusyn", "Últimas mudanças", "Mudança recente"} {
		if !strings.Contains(pack, want) {
			t.Errorf("pacote não contém %q", want)
		}
	}
}

func TestRenderContextPack_EmptySections(t *testing.T) {
	// Sem guidelines, perfil, wiki nem recentes → só o header, sem truncar.
	allOn := map[string]bool{"guidelines": true, "profile": true, "index": true, "recent": true}
	pack, truncated := renderContextPack(2, "", nil, profile.Profile{}, nil, nil, allOn, 1000)
	if truncated {
		t.Errorf("seções vazias não deveriam truncar")
	}
	if strings.Contains(pack, "Guidelines da org") || strings.Contains(pack, "Perfil do usuário") || strings.Contains(pack, "Índice") || strings.Contains(pack, "Últimas mudanças") {
		t.Errorf("seções vazias não deveriam aparecer: %q", pack)
	}
	if !strings.Contains(pack, "org 2") {
		t.Errorf("header da org ausente: %q", pack)
	}
}

func TestCtxClampRecent(t *testing.T) {
	cases := []struct {
		raw  string
		want int
	}{
		{"", ctxDefaultRecent},
		{"abc", ctxDefaultRecent},
		{"-1", ctxDefaultRecent},
		{"0", 0}, // "0" explícito desliga a seção
		{"3", 3}, // dentro da faixa
		{"999", ctxMaxRecent},
		{"  7 ", 7},
	}
	for _, c := range cases {
		if got := ctxClampRecent(c.raw); got != c.want {
			t.Errorf("ctxClampRecent(%q) = %d, want %d", c.raw, got, c.want)
		}
	}
}

func TestCtxParseSections(t *testing.T) {
	all := map[string]bool{"guidelines": true, "profile": true, "index": true, "recent": true}
	eq := func(a, b map[string]bool) bool {
		if len(a) != len(b) {
			return false
		}
		for k, v := range a {
			if b[k] != v {
				return false
			}
		}
		return true
	}
	cases := []struct {
		raw  string
		want map[string]bool
	}{
		{"", all},           // ausente → todas
		{"   ", all},        // só espaço → todas
		{"bogus,xpto", all}, // nenhum válido → todas
		{"guidelines,index", map[string]bool{"guidelines": true, "index": true}},
		{" GUIDELINES , recent ", map[string]bool{"guidelines": true, "recent": true}}, // case/espaço
		{"profile,bogus", map[string]bool{"profile": true}},                            // ignora inválido
	}
	for _, c := range cases {
		if got := ctxParseSections(c.raw); !eq(got, c.want) {
			t.Errorf("ctxParseSections(%q) = %v, want %v", c.raw, got, c.want)
		}
	}
}

func TestRenderContextPack_SectionsSubset(t *testing.T) {
	guidelines := []guideline.Item{{ID: 1, Title: "Regra", Content: "conteúdo da regra"}}
	prof := profile.Profile{Content: "meu perfil"}
	wiki := []contextWikiPage{{ID: 10, Title: "Pág wiki", Preview: "prev"}}
	recent := []contextRecentItem{{ID: 20, Title: "Mudança", Domain: "memory", CreatedAt: "2026-06-30"}}

	// Só guidelines + índice: perfil e recentes NÃO entram, mesmo com dados.
	only := map[string]bool{"guidelines": true, "index": true}
	pack, _ := renderContextPack(1, "p", guidelines, prof, wiki, recent, only, 4000)
	if !strings.Contains(pack, "Guidelines da org") || !strings.Contains(pack, "Índice do projeto") {
		t.Errorf("seções escolhidas deveriam aparecer: %q", pack)
	}
	if strings.Contains(pack, "Perfil do usuário") || strings.Contains(pack, "Últimas mudanças") {
		t.Errorf("seções não escolhidas vazaram: %q", pack)
	}
}

func TestCollapseWS(t *testing.T) {
	if got := collapseWS("a\n\n  b\tc  "); got != "a b c" {
		t.Errorf("collapseWS = %q, want %q", got, "a b c")
	}
}
