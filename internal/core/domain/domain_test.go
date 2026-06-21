package domain

import "testing"

func TestNormalizeInput_AceitaEntradaValida(t *testing.T) {
	for _, d := range []string{"memory", "knowledge", "guideline", "skill"} {
		if got := NormalizeInput(d); got != d {
			t.Errorf("NormalizeInput(%q) = %q; quer %q", d, got, d)
		}
	}
}

func TestNormalizeInput_RejeitaDerivadosECustom(t *testing.T) {
	// Anti forja de autoridade: derivados (saída do compile) e custom viram "memory".
	for _, d := range []string{"wiki", "lesson", "decision", "error", "project", "exploit", "<script>", ""} {
		if got := NormalizeInput(d); got != "memory" {
			t.Errorf("NormalizeInput(%q) = %q; quer \"memory\"", d, got)
		}
	}
}

func TestNormalizeInput_TrimELowercase(t *testing.T) {
	for _, d := range []string{"  MEMORY  ", "Guideline", "KNOWLEDGE"} {
		if got := NormalizeInput(d); !InputDomains[got] {
			t.Errorf("NormalizeInput(%q) = %q; não é domínio de entrada válido", d, got)
		}
	}
	// "DECISION" (derivado, maiúsculo) também deve cair pra memory.
	if got := NormalizeInput("DECISION"); got != "memory" {
		t.Errorf("NormalizeInput(DECISION) = %q; quer memory", got)
	}
}
