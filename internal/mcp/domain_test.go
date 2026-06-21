package mcp

import "testing"

// Protege o pipeline do compile: o agente só escreve em memory/knowledge/guideline;
// qualquer outro domínio (saída do compile ou custom) vira memory. Regressão do
// incidente 2026-06-12 (domain=project travou a destilação).
func TestNormalizeInputDomain(t *testing.T) {
	cases := map[string]string{
		"":            "memory",
		"memory":      "memory",
		"MEMORY":      "memory", // case-insensitive
		"  knowledge": "knowledge",
		"guideline":   "guideline",
		"project":     "memory", // o bug de hoje
		"wiki":        "memory", // saída do compile — agente não escreve
		"lesson":      "memory",
		"decision":    "memory",
		"error":       "memory",
		"meu-projeto": "memory", // custom arbitrário
	}
	for in, want := range cases {
		if got := normalizeInputDomain(in); got != want {
			t.Errorf("normalizeInputDomain(%q) = %q, want %q", in, got, want)
		}
	}
}
