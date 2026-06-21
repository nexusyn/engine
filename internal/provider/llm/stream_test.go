package llm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexusyn/engine/internal/config"
)

// streamStub é o equivalente de stubProvider para streaming tests.
type streamStub struct {
	name   string
	chunks []StreamChunk
	err    error
}

func (s *streamStub) Complete(_ context.Context, _ Prompt) (Result, error) {
	return Result{}, errors.New("streamStub: Complete não implementado")
}
func (s *streamStub) Name() string  { return s.name }
func (s *streamStub) Model() string { return s.name + "-model" }
func (s *streamStub) Stream(ctx context.Context, _ Prompt) (<-chan StreamChunk, error) {
	if s.err != nil {
		return nil, s.err
	}
	ch := make(chan StreamChunk, len(s.chunks))
	go func() {
		defer close(ch)
		for _, c := range s.chunks {
			select {
			case ch <- c:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

// drainStream coleta todos os chunks até fechamento, retornando deltas concatenadas e o último chunk Done.
func drainStream(t *testing.T, ch <-chan StreamChunk) (text string, last StreamChunk, err error) {
	t.Helper()
	for c := range ch {
		if c.Err != nil {
			return text, c, c.Err
		}
		text += c.Delta
		if c.Done {
			last = c
		}
	}
	return text, last, nil
}

func TestRouterStream_PrimarySucceeds(t *testing.T) {
	primary := &streamStub{name: "p", chunks: []StreamChunk{
		{Delta: "hello "},
		{Delta: "world"},
		{Done: true, TokensIn: 5, TokensOut: 2},
	}}
	fallback := &streamStub{name: "fb", err: errors.New("should not be called")}

	r := NewRouter(primary, fallback)
	ch, err := r.Stream(context.Background(), Prompt{User: "hi"})
	require.NoError(t, err)

	text, last, derr := drainStream(t, ch)
	require.NoError(t, derr)
	assert.Equal(t, "hello world", text)
	assert.True(t, last.Done)
	assert.Equal(t, 5, last.TokensIn)
}

func TestRouterStream_FallbackOn429(t *testing.T) {
	primary := &streamStub{name: "p", err: &HTTPError{Status: 429, Provider: "p"}}
	fallback := &streamStub{name: "fb", chunks: []StreamChunk{
		{Delta: "from-fb"},
		{Done: true},
	}}

	r := NewRouter(primary, fallback)
	ch, err := r.Stream(context.Background(), Prompt{User: "hi"})
	require.NoError(t, err)

	text, _, derr := drainStream(t, ch)
	require.NoError(t, derr)
	assert.Equal(t, "from-fb", text)
}

func TestRouterStream_NoStreamSupport(t *testing.T) {
	// stubProvider (não-streaming) — Router pula
	nonStream := &stubProvider{name: "x", res: Result{Content: "x"}}
	r := NewRouter(nonStream)
	_, err := r.Stream(context.Background(), Prompt{User: "hi"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrStreamNotSupported)
}

func TestRouterStream_NotRetryableAborts(t *testing.T) {
	primary := &streamStub{name: "p", err: &HTTPError{Status: 401, Provider: "p"}}
	fallback := &streamStub{name: "fb", chunks: []StreamChunk{{Delta: "x"}}}

	r := NewRouter(primary, fallback)
	_, err := r.Stream(context.Background(), Prompt{User: "hi"})
	require.Error(t, err)
	var httpErr *HTTPError
	require.ErrorAs(t, err, &httpErr)
	assert.Equal(t, 401, httpErr.Status)
}

// ───── HTTP integration via httptest ─────

const anthropicStreamBody = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","role":"assistant","content":[],"model":"claude-haiku-4-5","stop_reason":null,"usage":{"input_tokens":10,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"olá"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" mundo"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}

event: message_stop
data: {"type":"message_stop"}

`

func TestAnthropic_Stream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(anthropicStreamBody))
	}))
	defer srv.Close()

	p := NewAnthropic(config.ProviderConfig{
		APIKey:  "k",
		BaseURL: srv.URL,
		Timeout: 5 * time.Second,
	})

	ch, err := p.Stream(context.Background(), Prompt{User: "hi"})
	require.NoError(t, err)

	text, last, derr := drainStream(t, ch)
	require.NoError(t, derr)
	assert.Equal(t, "olá mundo", text)
	assert.True(t, last.Done)
	assert.Equal(t, 10, last.TokensIn)
	assert.Equal(t, 7, last.TokensOut)
	// Day 19 — usage metadata fields preenchidos
	assert.Equal(t, "anthropic", last.Provider)
	assert.Equal(t, "claude-haiku-4-5", last.Model)
	assert.GreaterOrEqual(t, last.LatencyMs, 0)
}

func TestAnthropic_Stream_429(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"rate"}`))
	}))
	defer srv.Close()

	p := NewAnthropic(config.ProviderConfig{APIKey: "k", BaseURL: srv.URL, Timeout: 5 * time.Second})
	_, err := p.Stream(context.Background(), Prompt{User: "hi"})
	require.Error(t, err)
	var httpErr *HTTPError
	require.ErrorAs(t, err, &httpErr)
	assert.Equal(t, 429, httpErr.Status)
}

const geminiStreamBody = `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"olá"}]}}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":1}}

data: {"candidates":[{"content":{"role":"model","parts":[{"text":" mundo"}]}}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":2}}

`

func TestGemini_Stream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Gemini API key vai na query string ?key=
		assert.NotEmpty(t, r.URL.Query().Get("key"))
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(geminiStreamBody))
	}))
	defer srv.Close()

	p := NewGemini(config.ProviderConfig{
		APIKey:  "k",
		BaseURL: srv.URL,
		Timeout: 5 * time.Second,
	})

	ch, err := p.Stream(context.Background(), Prompt{User: "hi"})
	require.NoError(t, err)

	text, last, derr := drainStream(t, ch)
	require.NoError(t, derr)
	assert.Equal(t, "olá mundo", text)
	assert.True(t, last.Done)
	assert.Equal(t, 8, last.TokensIn)
	assert.Equal(t, 2, last.TokensOut)
	// Day 19 — usage metadata fields preenchidos
	assert.Equal(t, "gemini", last.Provider)
	assert.NotEmpty(t, last.Model)
	assert.GreaterOrEqual(t, last.LatencyMs, 0)
}

func TestGemini_JSONMode_SetsResponseMimeType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		assert.Contains(t, string(body), "responseMimeType")
		assert.Contains(t, string(body), "application/json")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"{}"}]}}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1}}`))
	}))
	defer srv.Close()

	p := NewGemini(config.ProviderConfig{APIKey: "k", BaseURL: srv.URL, Timeout: 5 * time.Second})
	_, err := p.Complete(context.Background(), Prompt{User: "hi", JSONMode: true})
	require.NoError(t, err)
}

func TestGemini_NoJSONMode_OmitsResponseMimeType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		assert.NotContains(t, string(body), "responseMimeType")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"oi"}]}}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1}}`))
	}))
	defer srv.Close()

	p := NewGemini(config.ProviderConfig{APIKey: "k", BaseURL: srv.URL, Timeout: 5 * time.Second})
	_, err := p.Complete(context.Background(), Prompt{User: "hi"})
	require.NoError(t, err)
}

func TestAsStreaming(t *testing.T) {
	// AnthropicProvider implementa Streaming
	a := NewAnthropic(config.ProviderConfig{APIKey: "k"})
	assert.NotNil(t, AsStreaming(a))

	// stubProvider não implementa
	s := &stubProvider{name: "x"}
	assert.Nil(t, AsStreaming(s))
}
