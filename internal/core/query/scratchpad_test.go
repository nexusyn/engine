package query

import "testing"

func TestExtractFinal_Basic(t *testing.T) {
	in := "<scratch>event A 2026/01/01, event B 2026/02/01 -> 2 events</scratch><final>2</final>"
	if got := extractFinal(in); got != "2" {
		t.Errorf("got %q, want %q", got, "2")
	}
}

func TestExtractFinal_Whitespace(t *testing.T) {
	in := "<scratch>...</scratch>\n<final>\n  Korean, Italian, Thai.\n</final>"
	if got := extractFinal(in); got != "Korean, Italian, Thai." {
		t.Errorf("got %q", got)
	}
}

func TestExtractFinal_NoFinalTag_StripsScratch(t *testing.T) {
	// modelo esqueceu <final> mas deu a resposta depois do scratch
	in := "<scratch>enumerating mentions...</scratch>\nThe answer is 5."
	if got := extractFinal(in); got != "The answer is 5." {
		t.Errorf("got %q", got)
	}
}

func TestExtractFinal_NoTags(t *testing.T) {
	// modelo ignorou o formato -> devolve a resposta inteira intacta
	in := "38 coins."
	if got := extractFinal(in); got != "38 coins." {
		t.Errorf("got %q", got)
	}
}

func TestExtractFinal_NeverEmpty(t *testing.T) {
	for _, in := range []string{
		"<scratch>work</scratch><final></final>",
		"<scratch>only scratch, no final</scratch>",
		"<final>  </final>",
	} {
		if got := extractFinal(in); got == "" {
			t.Errorf("extractFinal(%q) devolveu vazio", in)
		}
	}
}

func TestExtractFinal_CaseInsensitive(t *testing.T) {
	in := "<SCRATCH>x</SCRATCH><FINAL>ok</FINAL>"
	if got := extractFinal(in); got != "ok" {
		t.Errorf("got %q", got)
	}
}
