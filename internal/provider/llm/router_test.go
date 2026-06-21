package llm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexusyn/engine/internal/config"
)

// stubProvider permite simular respostas determinísticas em tests do Router
// sem precisar subir HTTP servers.
type stubProvider struct {
	name string
	res  Result
	err  error
}

func (s *stubProvider) Complete(_ context.Context, _ Prompt) (Result, error) {
	if s.err != nil {
		return Result{}, s.err
	}
	return s.res, nil
}
func (s *stubProvider) Name() string  { return s.name }
func (s *stubProvider) Model() string { return s.name + "-model" }

func TestRouter_PrimarySucceeds(t *testing.T) {
	primary := &stubProvider{name: "p", res: Result{Content: "hello", Provider: "p"}}
	fallback := &stubProvider{name: "fb", err: errors.New("should not be called")}

	r := NewRouter(primary, fallback)
	res, err := r.Complete(context.Background(), Prompt{User: "hi"})

	require.NoError(t, err)
	assert.Equal(t, "hello", res.Content)
	assert.Equal(t, "p", res.Provider)
}

func TestRouter_FallbackOn429(t *testing.T) {
	primary := &stubProvider{name: "p", err: &HTTPError{Status: 429, Body: "rate limit", Provider: "p"}}
	fallback := &stubProvider{name: "fb", res: Result{Content: "ok", Provider: "fb"}}

	r := NewRouter(primary, fallback)
	res, err := r.Complete(context.Background(), Prompt{User: "hi"})

	require.NoError(t, err)
	assert.Equal(t, "ok", res.Content)
	assert.Equal(t, "fb", res.Provider)
}

func TestRouter_FallbackOn500(t *testing.T) {
	primary := &stubProvider{name: "p", err: &HTTPError{Status: 503, Body: "down", Provider: "p"}}
	fallback := &stubProvider{name: "fb", res: Result{Content: "ok", Provider: "fb"}}

	r := NewRouter(primary, fallback)
	res, err := r.Complete(context.Background(), Prompt{User: "hi"})

	require.NoError(t, err)
	assert.Equal(t, "fb", res.Provider)
}

func TestRouter_NoFallbackOn401(t *testing.T) {
	primary := &stubProvider{name: "p", err: &HTTPError{Status: 401, Body: "bad key", Provider: "p"}}
	fallback := &stubProvider{name: "fb", res: Result{Content: "ok", Provider: "fb"}}

	r := NewRouter(primary, fallback)
	_, err := r.Complete(context.Background(), Prompt{User: "hi"})

	require.Error(t, err)
	// 401 não é retryable — não deve tentar fallback
	var httpErr *HTTPError
	require.ErrorAs(t, err, &httpErr)
	assert.Equal(t, 401, httpErr.Status)
	assert.Equal(t, "p", httpErr.Provider)
}

func TestRouter_AllFail(t *testing.T) {
	p1 := &stubProvider{name: "p1", err: &HTTPError{Status: 503, Provider: "p1"}}
	p2 := &stubProvider{name: "p2", err: &HTTPError{Status: 500, Provider: "p2"}}
	p3 := &stubProvider{name: "p3", err: &HTTPError{Status: 502, Provider: "p3"}}

	r := NewRouter(p1, p2, p3)
	_, err := r.Complete(context.Background(), Prompt{User: "hi"})

	require.Error(t, err)
	// Último erro deve ser p3 (502)
	var httpErr *HTTPError
	require.ErrorAs(t, err, &httpErr)
	assert.Equal(t, "p3", httpErr.Provider)
}

func TestRouter_NoPrimaryOnlyFallback(t *testing.T) {
	// primary nil — Router usa apenas fallbacks
	fb := &stubProvider{name: "fb", res: Result{Content: "ok", Provider: "fb"}}
	r := NewRouter(nil, fb)
	res, err := r.Complete(context.Background(), Prompt{User: "hi"})

	require.NoError(t, err)
	assert.Equal(t, "fb", res.Provider)
}

func TestRouter_Empty(t *testing.T) {
	r := NewRouter(nil)
	_, err := r.Complete(context.Background(), Prompt{User: "hi"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "nenhum provider")
}

func TestIsRetryable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"429", &HTTPError{Status: 429}, true},
		{"408", &HTTPError{Status: 408}, true},
		{"500", &HTTPError{Status: 500}, true},
		{"503", &HTTPError{Status: 503}, true},
		{"400", &HTTPError{Status: 400}, false},
		{"401", &HTTPError{Status: 401}, false},
		{"404", &HTTPError{Status: 404}, false},
		{"context-deadline", context.DeadlineExceeded, true},
		{"errors.New plain", errors.New("random"), false},
		{"io-timeout", errors.New("read: i/o timeout"), true},
		{"connection-refused", errors.New("dial tcp: connection refused"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isRetryable(tc.err))
		})
	}
}

// ───── HTTP integration tests via httptest ─────

func TestAnthropic_Smoke(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "POST", r.Method)
		assert.Equal(t, "/v1/messages", r.URL.Path)
		assert.Equal(t, "test-key", r.Header.Get("x-api-key"))
		assert.NotEmpty(t, r.Header.Get("anthropic-version"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"content": [{"type": "text", "text": "olá"}],
			"usage": {"input_tokens": 10, "output_tokens": 5}
		}`))
	}))
	defer srv.Close()

	p := NewAnthropic(config.ProviderConfig{
		APIKey:  "test-key",
		Model:   "claude-haiku-4-5",
		BaseURL: srv.URL,
		Timeout: 5 * time.Second,
	})

	res, err := p.Complete(context.Background(), Prompt{System: "be brief", User: "hi"})
	require.NoError(t, err)
	assert.Equal(t, "olá", res.Content)
	assert.Equal(t, "anthropic", res.Provider)
	assert.Equal(t, 10, res.TokensIn)
	assert.Equal(t, 5, res.TokensOut)
}

func TestAnthropic_429_ReturnsHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"rate limited"}`))
	}))
	defer srv.Close()

	p := NewAnthropic(config.ProviderConfig{APIKey: "k", BaseURL: srv.URL, Timeout: 5 * time.Second})
	_, err := p.Complete(context.Background(), Prompt{User: "hi"})
	require.Error(t, err)

	var httpErr *HTTPError
	require.ErrorAs(t, err, &httpErr)
	assert.Equal(t, 429, httpErr.Status)
	assert.True(t, isRetryable(err))
}

func TestMiniMax_Smoke(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "POST", r.Method)
		assert.Equal(t, "/v1/text/chatcompletion_v2", r.URL.Path)
		assert.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices": [{"message": {"role": "assistant", "content": "olá"}}],
			"usage": {"prompt_tokens": 8, "completion_tokens": 3, "total_tokens": 11},
			"base_resp": {"status_code": 0, "status_msg": ""}
		}`))
	}))
	defer srv.Close()

	p := NewMiniMax(config.ProviderConfig{
		APIKey:  "test-key",
		Model:   "MiniMax-M1",
		BaseURL: srv.URL,
		Timeout: 5 * time.Second,
	})

	res, err := p.Complete(context.Background(), Prompt{System: "sys", User: "hi"})
	require.NoError(t, err)
	assert.Equal(t, "olá", res.Content)
	assert.Equal(t, "minimax", res.Provider)
	assert.Equal(t, 8, res.TokensIn)
	assert.Equal(t, 3, res.TokensOut)
}

func TestMiniMax_AppLevelError(t *testing.T) {
	// MiniMax retorna HTTP 200 com base_resp.status_code != 0 em erros de app
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices": [],
			"base_resp": {"status_code": 1004, "status_msg": "insufficient quota"}
		}`))
	}))
	defer srv.Close()

	p := NewMiniMax(config.ProviderConfig{APIKey: "k", BaseURL: srv.URL, Timeout: 5 * time.Second})
	_, err := p.Complete(context.Background(), Prompt{User: "hi"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "1004")
	assert.Contains(t, err.Error(), "insufficient quota")
}

func TestFactory_BuildsRouterFromConfig(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	cfg := config.LLMConfig{
		Primary:   "anthropic",
		Fallbacks: []string{"gemini", "minimax"},
		Anthropic: config.ProviderConfig{APIKey: "k", BaseURL: srv.URL, Timeout: 5 * time.Second},
		// Gemini sem key → pulado
		// MiniMax sem key → pulado
	}

	p, err := Factory(cfg)
	require.NoError(t, err)
	require.NotNil(t, p)

	// Router único provider (sem fallbacks ativos)
	res, err := p.Complete(context.Background(), Prompt{User: "hi"})
	require.NoError(t, err)
	assert.Equal(t, "ok", res.Content)
	assert.Equal(t, "anthropic", res.Provider)
}

func TestFactory_FailsWhenNoProviderKey(t *testing.T) {
	cfg := config.LLMConfig{
		Primary:   "anthropic",
		Fallbacks: []string{"gemini"},
		// Sem keys em ninguém
	}
	_, err := Factory(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nenhum provider")
}
