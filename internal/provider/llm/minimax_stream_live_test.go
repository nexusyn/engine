package llm

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexusyn/engine/internal/config"
)

// Live smoke do streaming MiniMax contra api.minimax.io. Só roda com
// MINIMAX_API_KEY no env (skip no CI): valida formato real do SSE,
// supressão de reasoning e mede TTFT.
//
//	MINIMAX_API_KEY=$(op read "op://EXAMPLE/minimax-key/credential") \
//	  go test ./internal/provider/llm/ -run TestMiniMax_Stream_Live -v
func TestMiniMax_Stream_Live(t *testing.T) {
	key := os.Getenv("MINIMAX_API_KEY")
	if key == "" {
		t.Skip("MINIMAX_API_KEY não setada — pulando live test")
	}

	p := NewMiniMax(config.ProviderConfig{
		APIKey:  key,
		Model:   "MiniMax-M2.7",
		Timeout: 120 * time.Second,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	start := time.Now()
	ch, err := p.Stream(ctx, Prompt{
		System:    "Answer concisely. Never show reasoning or work.",
		User:      "Name the two largest planets of the solar system, comma-separated, nothing else.",
		MaxTokens: 300,
	})
	require.NoError(t, err)

	var text string
	var ttft time.Duration
	var last StreamChunk
	for c := range ch {
		require.NoError(t, c.Err)
		if c.Delta != "" && ttft == 0 {
			ttft = time.Since(start)
		}
		text += c.Delta
		if c.Done {
			last = c
		}
	}

	t.Logf("TTFT=%s total=%s tokens_in=%d tokens_out=%d resposta=%q",
		ttft, time.Since(start), last.TokensIn, last.TokensOut, text)

	require.NotEmpty(t, text)
	low := strings.ToLower(text)
	assert.Contains(t, low, "jupiter")
	assert.Contains(t, low, "saturn")
	// Reasoning não pode vazar (nem tag nem CoT típico)
	assert.NotContains(t, low, "<think>")
	assert.NotContains(t, low, "the user is asking")
	assert.True(t, last.Done)
	assert.Equal(t, "minimax", last.Provider)
	assert.Greater(t, last.TokensOut, 0)
}
