package job

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// hashContent normaliza (trim + lowercase + colapsa whitespace) antes de hashear,
// pra que variações triviais sejam tratadas como a MESMA memória no dedup.
func TestHashContent_NormalizaCaseEWhitespace(t *testing.T) {
	base := hashContent("Luciano adora pizza")
	assert.Equal(t, base, hashContent("  luciano   ADORA pizza "), "case/whitespace não deve mudar o hash")
	assert.NotEqual(t, base, hashContent("Luciano adora sushi"), "conteúdo distinto deve mudar o hash")
	assert.Len(t, base, 32, "SHA-256 = 32 bytes")
}
