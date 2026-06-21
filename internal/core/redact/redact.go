// Package redact mascara segredos de ALTA CONFIANÇA no conteúdo ingerido, antes de
// persistir/chunkar/embeddar — pra que credenciais não virem chunk recuperável em
// texto claro (mitiga exfiltração via retrieval).
//
// Conservador de propósito: só formatos estruturados e inequívocos de credencial.
// Dados > código — não mutilamos prosa legítima com heurísticas frouxas tipo
// "password: <qualquer coisa>".
package redact

import "regexp"

// Mask substitui o segredo detectado.
const Mask = "«REDACTED-SECRET»"

var patterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-[A-Za-z0-9_\-]{20,}`),                                              // OpenAI (inclui sk-proj-)
	regexp.MustCompile(`\b(?:sk|pk|rk)_(?:live|test)_[A-Za-z0-9]{16,}\b`),                     // Stripe
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`),                                      // GitHub tokens
	regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`),                                    // Slack
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),                                                // AWS access key id
	regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{30,}\b`),                                         // Google API key
	regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{6,}\b`), // JWT
	regexp.MustCompile(`-----BEGIN (?:[A-Z ]+ )?PRIVATE KEY-----`),                            // chave privada PEM
}

// Redact mascara segredos de alta confiança. Retorna o texto tratado e quantos
// trechos foram mascarados (0 = nada mudou).
func Redact(s string) (string, int) {
	n := 0
	for _, re := range patterns {
		s = re.ReplaceAllStringFunc(s, func(string) string { n++; return Mask })
	}
	return s, n
}
