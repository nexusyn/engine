package compile

import (
	"strings"
	"testing"
)

func TestSanitize_DescartaSemSubstancia(t *testing.T) {
	in := Result{Pages: []Page{
		{Slug: "ok-page", Title: "Ok", Content: "conteúdo real"},
		{Slug: "vazio", Title: "Vazio", Content: "   "},          // sem conteúdo → descarta
		{Slug: "sem-titulo", Title: "", Content: "tem conteúdo"}, // sem título → descarta
	}}
	out := sanitize(in)
	if len(out.Pages) != 1 || out.Pages[0].Slug != "ok-page" {
		t.Fatalf("esperava 1 página válida; veio %d: %+v", len(out.Pages), out.Pages)
	}
}

func TestSanitize_ZeraSlugInvalido(t *testing.T) {
	in := Result{Pages: []Page{
		{Slug: "Slug Forjado!/../x", Title: "T", Content: "c"},
		{Slug: "valido-123", Title: "T2", Content: "c2"},
	}}
	out := sanitize(in)
	if out.Pages[0].Slug != "" {
		t.Errorf("slug inválido devia virar \"\" (persistPages regenera); veio %q", out.Pages[0].Slug)
	}
	if out.Pages[1].Slug != "valido-123" {
		t.Errorf("slug válido devia ser mantido; veio %q", out.Pages[1].Slug)
	}
}

func TestSanitize_LimitaPaginasEConteudo(t *testing.T) {
	var pages []Page
	for i := 0; i < maxCompilePages+10; i++ {
		pages = append(pages, Page{Slug: "p", Title: "t", Content: "c"})
	}
	pages = append([]Page{{Slug: "big", Title: "big", Content: strings.Repeat("x", maxCompilePageRunes+500)}}, pages...)
	out := sanitize(Result{Pages: pages})
	if len(out.Pages) > maxCompilePages {
		t.Errorf("nº de páginas não limitado: %d > %d", len(out.Pages), maxCompilePages)
	}
	if rs := []rune(out.Pages[0].Content); len(rs) > maxCompilePageRunes {
		t.Errorf("conteúdo não truncado: %d runas", len(rs))
	}
}
