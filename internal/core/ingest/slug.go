package ingest

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"unicode"
)

// MaxSlugLen é o limite de caracteres no slug final (sem o sufixo random).
const MaxSlugLen = 80

// Slug gera um slug URL-safe a partir de um título.
//
// Formato: "<title-normalizado>-<6-hex>"
//
// Normalização:
//   - lowercase
//   - remove acentos
//   - troca não-alfanumérico por hyphen
//   - collapse hyphens consecutivos
//   - trim leading/trailing hyphens
//   - max MaxSlugLen chars antes do sufixo
//
// Sufixo random de 6 hex evita colisão sem precisar consultar DB.
func Slug(title string) string {
	base := Normalize(title)
	if base == "" {
		base = "untitled"
	}
	if len(base) > MaxSlugLen {
		base = base[:MaxSlugLen]
	}
	base = strings.TrimRight(base, "-")
	return base + "-" + randomSuffix(3) // 3 bytes = 6 hex chars
}

// Normalize converte string pra forma slug-safe (lowercase, sem acentos,
// hyphen-separated, sem caracteres não-alfanuméricos). Exportada pra uso
// em entities (dedup por slug canônico, sem sufixo random).
func Normalize(s string) string {
	s = strings.ToLower(s)

	var b strings.Builder
	b.Grow(len(s))
	lastHyphen := false
	for _, r := range s {
		// Mapeamento simples de acentos comuns pt-BR
		r = stripAccent(r)
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastHyphen = false
		} else if !lastHyphen && b.Len() > 0 {
			b.WriteByte('-')
			lastHyphen = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}

// stripAccent faz mapeamento manual de chars acentuados PT-BR comuns pra ASCII.
// Solução simples sem dep de unicode/norm — cobre 95% dos casos brasileiros.
func stripAccent(r rune) rune {
	switch r {
	case 'á', 'à', 'â', 'ã', 'ä':
		return 'a'
	case 'é', 'è', 'ê', 'ë':
		return 'e'
	case 'í', 'ì', 'î', 'ï':
		return 'i'
	case 'ó', 'ò', 'ô', 'õ', 'ö':
		return 'o'
	case 'ú', 'ù', 'û', 'ü':
		return 'u'
	case 'ç':
		return 'c'
	case 'ñ':
		return 'n'
	}
	if unicode.Is(unicode.M, r) {
		return ' ' // marca de combining → vira espaço (que vira hyphen depois)
	}
	return r
}

// randomSuffix gera N bytes random em hex.
func randomSuffix(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// Fallback determinístico se entropia indisponível (extremamente raro)
		return "000000"[:n*2]
	}
	return hex.EncodeToString(b)
}
