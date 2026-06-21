package auth

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateToken_FormatAndEntropy(t *testing.T) {
	r1, h1, err := GenerateToken()
	require.NoError(t, err)
	r2, h2, err := GenerateToken()
	require.NoError(t, err)

	// 32 bytes em hex = 64 chars
	assert.Len(t, r1, 64, "random_hex deve ter 64 chars")
	assert.Len(t, r2, 64)

	// SHA-256 em hex = 64 chars
	assert.Len(t, h1, 64, "hash hex deve ter 64 chars")

	// Dois tokens consecutivos devem diferir (entropia real)
	assert.NotEqual(t, r1, r2, "tokens consecutivos não podem repetir")
	assert.NotEqual(t, h1, h2, "hashes não podem repetir")
}

func TestParseToken_ValidFormat(t *testing.T) {
	random := strings.Repeat("a", 64)
	raw := "42|" + random

	parsed, err := ParseToken(raw)
	require.NoError(t, err)
	assert.Equal(t, int64(42), parsed.ID)
	assert.Equal(t, random, parsed.RandomHex)
	assert.Len(t, parsed.RandomHash, 64)
}

func TestParseToken_InvalidFormats(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"empty", ""},
		{"no pipe", "abcdef"},
		{"id não-numérico", "abc|" + strings.Repeat("a", 64)},
		{"id zero", "0|" + strings.Repeat("a", 64)},
		{"id negativo", "-1|" + strings.Repeat("a", 64)},
		{"random curto", "1|abc"},
		{"random longo", "1|" + strings.Repeat("a", 65)},
		{"só pipe", "|"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseToken(tc.raw)
			assert.ErrorIs(t, err, ErrInvalidTokenFormat)
		})
	}
}

func TestParseToken_StripsWhitespace(t *testing.T) {
	random := strings.Repeat("b", 64)
	parsed, err := ParseToken("  7|" + random + "  \n")
	require.NoError(t, err)
	assert.Equal(t, int64(7), parsed.ID)
}

func TestStripBearer(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"Bearer abc", "abc"},
		{"Bearer  abc  ", "abc"},
		{"abc", ""},
		{"", ""},
		{"bearer lowercase", ""}, // case-sensitive por design (RFC)
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, StripBearer(tc.input))
	}
}

func TestCompareHashes_ConstantTime(t *testing.T) {
	// Não medimos timing aqui (não-determinístico), só correção semântica
	a := strings.Repeat("a", 64)
	b := strings.Repeat("a", 64)
	c := strings.Repeat("b", 64)

	assert.True(t, CompareHashes(a, b))
	assert.False(t, CompareHashes(a, c))
	assert.False(t, CompareHashes(a, "shorter"))
}

func TestFormatToken_RoundTrip(t *testing.T) {
	random, _, err := GenerateToken()
	require.NoError(t, err)

	formatted := FormatToken(123, random)
	parsed, err := ParseToken(formatted)
	require.NoError(t, err)
	assert.Equal(t, int64(123), parsed.ID)
	assert.Equal(t, random, parsed.RandomHex)
}

func TestTokenRecord_HasAbility(t *testing.T) {
	tr := &TokenRecord{Abilities: []string{"read", "write"}}
	assert.True(t, tr.HasAbility("read"))
	assert.True(t, tr.HasAbility("write"))
	assert.False(t, tr.HasAbility("admin"))

	wildcard := &TokenRecord{Abilities: []string{"*"}}
	assert.True(t, wildcard.HasAbility("anything"))
	assert.True(t, wildcard.HasAbility("admin"))
}
