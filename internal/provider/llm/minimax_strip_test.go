package llm

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestStripThinking_RemovesBlock(t *testing.T) {
	in := "<think>\nThe user is asking...\n</think>\n\nLuna tem 4 anos."
	out := stripThinking(in)
	assert.Equal(t, "Luna tem 4 anos.", out)
}

func TestStripThinking_MultipleBlocks(t *testing.T) {
	in := "<think>step 1</think>Hello <think>step 2</think>World"
	out := stripThinking(in)
	assert.Equal(t, "Hello World", out)
}

func TestStripThinking_UnterminatedBlock(t *testing.T) {
	// LLM truncated mid-thinking → drop tudo a partir do <think> aberto
	in := "Some answer <think>chain of thought that never closes..."
	out := stripThinking(in)
	assert.Equal(t, "Some answer", out)
}

func TestStripThinking_NoBlocks(t *testing.T) {
	in := "  Just a clean answer.  "
	out := stripThinking(in)
	assert.Equal(t, "Just a clean answer.", out)
}

func TestStripThinking_EmptyString(t *testing.T) {
	assert.Equal(t, "", stripThinking(""))
	assert.Equal(t, "", stripThinking("<think></think>"))
}
