package job

import (
	"testing"

	"github.com/nexusyn/engine/internal/core/compile"
)

// TestGroupByProject — o agrupamento das fontes por project preserva a ordem de
// primeira aparição de cada project e particiona corretamente (global = ""). É a
// peça que dá a cada derivado o project das suas memórias-fonte (proveniência),
// fechando o vazamento do filtro `project=` no conhecimento destilado.
func TestGroupByProject(t *testing.T) {
	src := []compile.Doc{
		{ID: 1, Project: "nexusyn"},
		{ID: 2, Project: ""},
		{ID: 3, Project: "reachyn"},
		{ID: 4, Project: "nexusyn"},
		{ID: 5, Project: ""},
	}
	groups := groupByProject(src)

	if len(groups) != 3 {
		t.Fatalf("esperava 3 grupos, veio %d", len(groups))
	}
	// ordem de primeira aparição: nexusyn, global, reachyn
	if groups[0].project != "nexusyn" || groups[1].project != "" || groups[2].project != "reachyn" {
		t.Fatalf("ordem dos grupos errada: %q, %q, %q", groups[0].project, groups[1].project, groups[2].project)
	}
	// partição por project, ordem interna preservada
	if len(groups[0].docs) != 2 || groups[0].docs[0].ID != 1 || groups[0].docs[1].ID != 4 {
		t.Errorf("grupo nexusyn deveria ter ids [1 4], veio %+v", groups[0].docs)
	}
	if len(groups[1].docs) != 2 || groups[1].docs[0].ID != 2 || groups[1].docs[1].ID != 5 {
		t.Errorf("grupo global deveria ter ids [2 5], veio %+v", groups[1].docs)
	}
	if len(groups[2].docs) != 1 || groups[2].docs[0].ID != 3 {
		t.Errorf("grupo reachyn deveria ter id [3], veio %+v", groups[2].docs)
	}
}

func TestGroupByProject_Vazio(t *testing.T) {
	if g := groupByProject(nil); len(g) != 0 {
		t.Fatalf("nil sources → 0 grupos, veio %d", len(g))
	}
}

func TestGroupByProject_UmProjeto(t *testing.T) {
	src := []compile.Doc{{ID: 1, Project: "nexusyn"}, {ID: 2, Project: "nexusyn"}}
	g := groupByProject(src)
	if len(g) != 1 || g[0].project != "nexusyn" || len(g[0].docs) != 2 {
		t.Fatalf("um só projeto → 1 grupo com 2 docs, veio %+v", g)
	}
}
