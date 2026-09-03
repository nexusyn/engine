package mcp

import (
	"context"
	"testing"

	"github.com/nexusyn/engine/internal/tenant"
)

// ctxWith monta um contexto com as abilities dadas (como o auth.Middleware faz).
func ctxWith(abilities ...string) context.Context {
	return tenant.WithAbilities(context.Background(), abilities)
}

// hasFlag testa pertencimento EXATO (deny-flags aditivas). CRÍTICO: o curinga "*"
// NÃO pode casar "no_delete" — senão um token operador (com "*") seria barrado de
// apagar. Só quem traz a flag literal é barrado.
func TestHasFlag(t *testing.T) {
	cases := []struct {
		name      string
		abilities []string
		want      bool
	}{
		{"token com no_delete", []string{"read", "write", "no_delete"}, true},
		{"wildcard NÃO casa no_delete", []string{"*"}, false},
		{"write normal não tem flag", []string{"read", "write"}, false},
		{"vazio", nil, false},
	}
	for _, c := range cases {
		if got := hasFlag(ctxWith(c.abilities...), "no_delete"); got != c.want {
			t.Errorf("%s: hasFlag(no_delete) = %v, want %v", c.name, got, c.want)
		}
	}
}

// envBool: default quando vazio, e parsing de truthy.
func TestEnvBool(t *testing.T) {
	if !envBool("MCP_NOPE_XYZ", true) {
		t.Error("envBool vazio deveria usar o default true")
	}
	if envBool("MCP_NOPE_XYZ", false) {
		t.Error("envBool vazio deveria usar o default false")
	}
}

// hasWrite gateia as tools de mutação (add/update/delete). Tokens read-only (sem
// "write") devem ser barrados; "*" passa em tudo. Regressão do furo de privilégio:
// antes, qualquer token MCP tinha CRUD total (nenhum gate por tool).
func TestHasWrite(t *testing.T) {
	cases := []struct {
		name      string
		abilities []string
		want      bool
	}{
		{"client default tem write", []string{"ingest", "query", "search", "mcp", "read", "write"}, true},
		{"read-only nega", []string{"read", "search", "query"}, false},
		{"wildcard passa", []string{"*"}, true},
		{"vazio nega", nil, false},
		{"mcp sozinho nega", []string{"mcp"}, false},
	}
	for _, c := range cases {
		if got := hasWrite(ctxWith(c.abilities...)); got != c.want {
			t.Errorf("%s: hasWrite(%v) = %v, want %v", c.name, c.abilities, got, c.want)
		}
	}
}

// clampSearchLimit: default 20 (sweet spot validado, = /v1/query e LME_LIMIT do
// bench), teto 100 — o teto barra limit gigante que faria a busca vetorial/rerank
// explodir (DoS de custo). (R1)
func TestClampSearchLimit(t *testing.T) {
	cases := map[int]int{
		0:       20,  // default
		-7:      20,  // negativo → default
		1:       1,   // dentro da faixa
		50:      50,  // dentro da faixa
		100:     100, // no teto
		101:     100, // acima → teto
		1000000: 100, // gigante → teto
	}
	for in, want := range cases {
		if got := clampSearchLimit(in); got != want {
			t.Errorf("clampSearchLimit(%d) = %d, want %d", in, got, want)
		}
	}
}

// auditKind: guideline (control-plane) recebe kind dedicado pra ser alertável à
// parte das memórias comuns. (R3)
func TestAuditKind(t *testing.T) {
	cases := []struct{ domain, action, want string }{
		{"memory", "added", "memory.added"},
		{"knowledge", "updated", "memory.updated"},
		{"guideline", "added", "guideline.added"},
		{"guideline", "deleted", "guideline.deleted"},
		{"", "updated", "memory.updated"},
	}
	for _, c := range cases {
		if got := auditKind(c.domain, c.action); got != c.want {
			t.Errorf("auditKind(%q, %q) = %q, want %q", c.domain, c.action, got, c.want)
		}
	}
}
