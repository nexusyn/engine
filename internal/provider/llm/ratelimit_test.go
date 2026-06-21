package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexusyn/engine/internal/config"
)

func TestNewLimiter_NilWhenZero(t *testing.T) {
	assert.Nil(t, newLimiter(0))
	assert.Nil(t, newLimiter(-5))
}

func TestNewLimiter_AllowsBurst(t *testing.T) {
	l := newLimiter(10)
	require.NotNil(t, l)
	// Burst inicial = 10 → 10 takes imediatos OK
	for i := 0; i < 10; i++ {
		assert.True(t, l.Allow(), "take %d deve passar com burst", i)
	}
	// 11º não passa imediatamente
	assert.False(t, l.Allow(), "burst esgotado")
}

func TestWaitLimiter_NilLimiter_ReturnsImmediately(t *testing.T) {
	err := waitLimiter(context.Background(), nil)
	assert.NoError(t, err)
}

func TestWaitLimiter_RespectsContext(t *testing.T) {
	// Esgota o burst, depois espera com ctx cancelado → erro rápido
	l := newLimiter(1) // burst=1
	require.True(t, l.Allow())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := waitLimiter(ctx, l)
	assert.ErrorIs(t, err, context.Canceled)
}

// TestProvider_RateLimit_RealUsage simula chamadas rápidas a um provider
// com RPS=2 e verifica que o limiter introduz delay nas chamadas extras.
func TestProvider_RateLimit_BlocksUntilSlot(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"content":[{"type":"text","text":"ok"}],
			"usage":{"input_tokens":1,"output_tokens":1}
		}`))
	}))
	defer srv.Close()

	// RPS=2, burst=2 — 3ª request deve aguardar ~500ms (1s/2)
	p := NewAnthropic(config.ProviderConfig{
		APIKey:       "k",
		BaseURL:      srv.URL,
		Timeout:      5 * time.Second,
		RateLimitRPS: 2,
	})

	start := time.Now()
	for i := 0; i < 3; i++ {
		_, err := p.Complete(context.Background(), Prompt{User: "hi"})
		require.NoError(t, err)
	}
	elapsed := time.Since(start)

	assert.Equal(t, int32(3), atomic.LoadInt32(&hits))
	// 3 requests com burst=2 + 1 wait → ≥ 400ms (margem por jitter de CI)
	assert.GreaterOrEqual(t, elapsed.Milliseconds(), int64(400),
		"3ª chamada deve esperar ~500ms do limiter, total >= 400ms")
}

func TestProvider_NoRateLimit_NoDelay(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	// RateLimitRPS=0 → sem limiter
	p := NewAnthropic(config.ProviderConfig{
		APIKey:  "k",
		BaseURL: srv.URL,
		Timeout: 5 * time.Second,
	})

	start := time.Now()
	for i := 0; i < 5; i++ {
		_, err := p.Complete(context.Background(), Prompt{User: "hi"})
		require.NoError(t, err)
	}
	elapsed := time.Since(start)

	// 5 calls sem rate limit devem rodar em << 1s (cada httptest ~ms)
	assert.Less(t, elapsed.Milliseconds(), int64(500),
		"sem rate limit, 5 chamadas devem ser rápidas (<500ms)")
}
