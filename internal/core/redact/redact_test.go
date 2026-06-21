package redact

import (
	"strings"
	"testing"
)

func TestRedact_MascaraSegredos(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"openai", "minha chave é sk-proj-ABCDEFGHIJKLMNOPQRSTUVWX1234567890 ok"},
		{"stripe", "use sk_live_abcdef1234567890ABCDEF agora"},
		{"github", "token ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"},
		{"aws", "id AKIAIOSFODNN7EXAMPLE fim"},
		{"google", "key AIzaSyA1234567890abcdefABCDEF_ghijklmnop"},
		{"jwt", "bearer eyJhbGciOiJIUzI1Ni1.eyJzdWIiOiIxMjM0NTY.SflKxwRJSMeKKF2QT4"},
		{"pem", "-----BEGIN RSA PRIVATE KEY-----"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, n := Redact(c.in)
			if n == 0 {
				t.Fatalf("%s: nada mascarado em %q", c.name, c.in)
			}
			if !strings.Contains(out, Mask) {
				t.Errorf("%s: saída sem máscara: %q", c.name, out)
			}
		})
	}
}

func TestRedact_NaoMutilaProsaLegitima(t *testing.T) {
	// Conservador: texto normal não pode ser tocado (dados > código).
	for _, s := range []string{
		"A reunião foi ótima e decidimos usar o plano Pro.",
		"O preço subiu para 37 dólares em março de 2026.",
		"sk é o início de muitas palavras como skate e skill.",
	} {
		out, n := Redact(s)
		if n != 0 || out != s {
			t.Errorf("prosa legítima alterada: %q -> %q (n=%d)", s, out, n)
		}
	}
}
