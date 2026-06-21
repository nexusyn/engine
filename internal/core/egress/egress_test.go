package egress

import (
	"strings"
	"testing"
)

func TestFilter_RemoveURLNaoAncorada(t *testing.T) {
	answer := "Veja em http://exfil.example.com/?d=segredo agora"
	grounding := "pergunta do usuário sobre suas memórias legítimas"
	out, n := Filter(answer, grounding)
	if n != 1 {
		t.Fatalf("esperava 1 URL removida; veio %d", n)
	}
	if strings.Contains(out, "exfil.example.com") {
		t.Errorf("URL de exfil não removida: %q", out)
	}
	if !strings.Contains(out, Mask) {
		t.Errorf("máscara ausente: %q", out)
	}
}

func TestFilter_MantemURLAncorada(t *testing.T) {
	answer := "Seu site é https://example.com/blog"
	grounding := "o usuário guardou que o site dele é https://example.com"
	out, n := Filter(answer, grounding)
	if n != 0 || out != answer {
		t.Errorf("URL legítima (host nas fontes) não devia ser tocada: %q (n=%d)", out, n)
	}
}

func TestFilter_SemURL(t *testing.T) {
	answer := "A resposta é 42."
	out, n := Filter(answer, "qualquer grounding")
	if n != 0 || out != answer {
		t.Errorf("texto sem URL não devia mudar: %q (n=%d)", out, n)
	}
}

func TestFilter_HostComPortaEUserinfo(t *testing.T) {
	// host presente nas fontes mesmo com porta/userinfo na URL da resposta → mantém.
	answer := "acesse http://user:pass@api.exemplo.io:8080/x"
	grounding := "fonte menciona api.exemplo.io"
	if _, n := Filter(answer, grounding); n != 0 {
		t.Errorf("host ancorado (com porta/userinfo) não devia ser removido")
	}
}
