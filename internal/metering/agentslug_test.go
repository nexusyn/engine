package metering

import "testing"

func TestSanitizeAgentSlug(t *testing.T) {
	cases := map[string]string{
		"claude":     "claude",   // legítimo inalterado
		"Codex":      "codex",    // lowercase
		"  cursor  ": "cursor",   // trim
		"hermes_1":   "hermes_1", // _ e dígitos ok
		"../admin":   "admin",    // path traversal neutralizado
		"<script>":   "script",   // tags neutralizadas
		"a b/c":      "a-b-c",    // chars inválidos viram '-'
		"":           "",         // vazio
	}
	for in, want := range cases {
		if got := SanitizeAgentSlug(in); got != want {
			t.Errorf("SanitizeAgentSlug(%q) = %q; quer %q", in, got, want)
		}
	}
}
