package llm

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexusyn/engine/internal/config"
)

// ───── thinkStripper (strip incremental de <think>) ─────

// feedAll alimenta o stripper com os pedaços e devolve a saída concatenada + flush.
func feedAll(parts ...string) string {
	st := &thinkStripper{}
	var out string
	for _, p := range parts {
		out += st.feed(p)
	}
	return out + st.flush()
}

func TestThinkStripper_NoTags(t *testing.T) {
	assert.Equal(t, "Luna tem 4 anos.", feedAll("Luna tem ", "4 anos."))
}

func TestThinkStripper_BlockInOneChunk(t *testing.T) {
	assert.Equal(t, "Luna tem 4 anos.", feedAll("<think>raciocínio</think>\n\nLuna tem 4 anos."))
}

func TestThinkStripper_TagSplitAcrossChunks(t *testing.T) {
	// pior caso: as DUAS tags chegam partidas entre chunks
	assert.Equal(t, "resposta", feedAll("<th", "ink>cot interno</th", "ink>resposta"))
}

func TestThinkStripper_SplitCharByChar(t *testing.T) {
	in := "<think>x</think>ok"
	st := &thinkStripper{}
	var out string
	for _, r := range in {
		out += st.feed(string(r))
	}
	out += st.flush()
	assert.Equal(t, "ok", out)
}

func TestThinkStripper_MultipleBlocks(t *testing.T) {
	assert.Equal(t, "Hello World", feedAll("<think>step 1</think>Hello ", "<think>step 2</think>World"))
}

func TestThinkStripper_UnterminatedDropsTail(t *testing.T) {
	// <think> aberto até o EOF → descarta (paridade com stripThinking)
	assert.Equal(t, "Some answer", feedAll("Some answer <think>never closes..."))
}

func TestThinkStripper_FalsePartialTagIsEmitted(t *testing.T) {
	// "<thin" no fim de chunk fica retido; quando o próximo chunk prova que
	// NÃO era tag, o texto sai intacto.
	assert.Equal(t, "a <thinker b", feedAll("a <thin", "ker b"))
	// e no flush: sufixo ambíguo que nunca virou tag também sai
	assert.Equal(t, "a <thin", feedAll("a <thin"))
}

func TestThinkStripper_LeadingWhitespaceTrimmed(t *testing.T) {
	assert.Equal(t, "resposta", feedAll("<think>cot</think>", "\n\n", "resposta"))
}

// ───── MiniMaxProvider.Stream (HTTP via httptest) ─────

const minimaxStreamBody = `data: {"choices":[{"delta":{"role":"assistant","reasoning_content":"pensando..."}}],"base_resp":{"status_code":0}}

data: {"choices":[{"delta":{"content":"<think>cot "}}]}

data: {"choices":[{"delta":{"content":"interno</think>olá"}}]}

data: {"choices":[{"delta":{"content":" mundo"}}]}

data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":9}}

data: [DONE]

`

func TestMiniMax_Stream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		assert.Contains(t, string(body), `"stream":true`)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(minimaxStreamBody))
	}))
	defer srv.Close()

	p := NewMiniMax(config.ProviderConfig{
		APIKey:  "k",
		Model:   "MiniMax-M2.7",
		BaseURL: srv.URL,
		Timeout: 5 * time.Second,
	})

	ch, err := p.Stream(context.Background(), Prompt{User: "hi"})
	require.NoError(t, err)

	text, last, derr := drainStream(t, ch)
	require.NoError(t, derr)
	assert.Equal(t, "olá mundo", text) // reasoning_content e <think> suprimidos
	assert.True(t, last.Done)
	assert.Equal(t, 12, last.TokensIn)
	assert.Equal(t, 9, last.TokensOut)
	assert.Equal(t, "minimax", last.Provider)
	assert.Equal(t, "MiniMax-M2.7", last.Model)
	assert.GreaterOrEqual(t, last.LatencyMs, 0)
}

func TestMiniMax_Stream_429(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"rate"}`))
	}))
	defer srv.Close()

	p := NewMiniMax(config.ProviderConfig{APIKey: "k", BaseURL: srv.URL, Timeout: 5 * time.Second})
	_, err := p.Stream(context.Background(), Prompt{User: "hi"})
	require.Error(t, err)
	var httpErr *HTTPError
	require.ErrorAs(t, err, &httpErr)
	assert.Equal(t, 429, httpErr.Status)
}

func TestMiniMax_Stream_AppErrMidStream(t *testing.T) {
	// MiniMax sinaliza erro de app via base_resp mesmo com HTTP 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"base_resp":{"status_code":1002,"status_msg":"rate limited"}}` + "\n\n"))
	}))
	defer srv.Close()

	p := NewMiniMax(config.ProviderConfig{APIKey: "k", BaseURL: srv.URL, Timeout: 5 * time.Second})
	ch, err := p.Stream(context.Background(), Prompt{User: "hi"})
	require.NoError(t, err)

	_, _, derr := drainStream(t, ch)
	require.Error(t, derr)
	assert.Contains(t, derr.Error(), "1002")
}

func TestMiniMax_ImplementsStreaming(t *testing.T) {
	p := NewMiniMax(config.ProviderConfig{APIKey: "k"})
	assert.NotNil(t, AsStreaming(p))
}
