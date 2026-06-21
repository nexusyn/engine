package ingest

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChunk_SmallText_SingleChunk(t *testing.T) {
	chunks := ChunkText("Olá mundo.", DefaultChunkConfig())
	require.Len(t, chunks, 1)
	assert.Equal(t, 0, chunks[0].Position)
	assert.Equal(t, "Olá mundo.", chunks[0].Content)
}

func TestChunk_EmptyText(t *testing.T) {
	chunks := ChunkText("", DefaultChunkConfig())
	require.Len(t, chunks, 1)
	assert.Empty(t, chunks[0].Content)
}

func TestChunk_DisabledViaZeroMaxChars(t *testing.T) {
	bigText := strings.Repeat("foo bar ", 1000)
	chunks := ChunkText(bigText, ChunkConfig{MaxChars: 0})
	require.Len(t, chunks, 1, "MaxChars=0 retorna single chunk")
}

func TestChunk_BoundariesAreNatural(t *testing.T) {
	// Texto com parágrafos óbvios — chunks devem cortar em \n\n
	text := strings.Repeat("Esta é uma frase.\n\n", 50) // ~950 chars, 50 paragraphs
	chunks := ChunkText(text, ChunkConfig{MaxChars: 200, Overlap: 30, MinChars: 20})

	require.Greater(t, len(chunks), 1)
	// Verifica positions ordenados
	for i, c := range chunks {
		assert.Equal(t, i, c.Position, "position deve ser ordenada")
	}
}

func TestChunk_OverlapPreservaContexto(t *testing.T) {
	// String sem boundaries naturais — força corte hard, overlap deve aparecer
	text := strings.Repeat("a", 500)
	chunks := ChunkText(text, ChunkConfig{MaxChars: 100, Overlap: 30, MinChars: 20})

	require.GreaterOrEqual(t, len(chunks), 4)
	// Overlap: cada chunk após o primeiro deve ter os ultimos 30 chars do anterior
	for i := 1; i < len(chunks); i++ {
		prevTail := chunks[i-1].Content[len(chunks[i-1].Content)-30:]
		assert.Contains(t, chunks[i].Content, prevTail[:20])
	}
}

func TestChunk_SmallTailMergedIntoPrevious(t *testing.T) {
	// Configura pra forçar último chunk pequeno
	text := strings.Repeat("a", 1505) // 5 chars sobra após MaxChars=500
	chunks := ChunkText(text, ChunkConfig{MaxChars: 500, Overlap: 0, MinChars: 50})

	// Último chunk não deve ser <50 chars
	last := chunks[len(chunks)-1]
	assert.GreaterOrEqual(t, len(last.Content), 50, "merge de cauda pequena")
}

// ───── slug tests ─────

func TestSlug_RemoveAcentos(t *testing.T) {
	s := Slug("Açúcar e Carinho")
	assert.True(t, strings.HasPrefix(s, "acucar-e-carinho-"), "got: %s", s)
}

func TestSlug_TrocaEspacoPorHyphen(t *testing.T) {
	s := Slug("Hello World 2026")
	assert.True(t, strings.HasPrefix(s, "hello-world-2026-"), "got: %s", s)
}

func TestSlug_NormalizaPontuacao(t *testing.T) {
	s := Slug("Tudo bem?? Não!")
	assert.True(t, strings.HasPrefix(s, "tudo-bem-nao-"), "got: %s", s)
}

func TestSlug_Empty(t *testing.T) {
	s := Slug("")
	assert.True(t, strings.HasPrefix(s, "untitled-"), "got: %s", s)
}

func TestSlug_OnlyPunctuation(t *testing.T) {
	s := Slug("!!!???")
	assert.True(t, strings.HasPrefix(s, "untitled-"), "got: %s", s)
}

func TestSlug_RandomSuffix(t *testing.T) {
	// 2 chamadas com mesmo input geram slugs diferentes (sufixo aleatório)
	s1 := Slug("teste")
	s2 := Slug("teste")
	assert.NotEqual(t, s1, s2, "sufixo random deve diferir")
}

func TestSlug_TruncatesLongTitle(t *testing.T) {
	long := strings.Repeat("a", 200)
	s := Slug(long)
	// 80 chars base + "-" + 6 hex = 87 max
	assert.LessOrEqual(t, len(s), MaxSlugLen+10)
}
