package api

import "testing"

func TestValidateBaseURL_PermiteLegitimos(t *testing.T) {
	ok := []string{
		"",                       // default do provider
		"https://api.minimax.io", // provedor conhecido
		"https://generativelanguage.googleapis.com",
		"http://reranker:80",       // serviço docker (sem ponto)
		"http://10.20.0.5:8080",    // IP privado (self-hosted)
		"http://127.0.0.1:11434",   // loopback
		"https://gw.minimax.io/v1", // subdomínio de provedor
	}
	for _, u := range ok {
		if err := validateBaseURL(u); err != nil {
			t.Errorf("validateBaseURL(%q) deveria PERMITIR, veio erro: %v", u, err)
		}
	}
}

func TestValidateBaseURL_BloqueiaHostPublicoArbitrario(t *testing.T) {
	bad := []string{
		"https://exfil.example.com", // host público arbitrário (MITM)
		"http://attacker.evil/v1",
		"https://8.8.8.8",      // IP público
		"ftp://api.minimax.io", // scheme inválido
	}
	for _, u := range bad {
		if err := validateBaseURL(u); err == nil {
			t.Errorf("validateBaseURL(%q) deveria BLOQUEAR, mas passou", u)
		}
	}
}
