package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/nexusyn/engine/internal/config"
)

// MiniMaxProvider chama api.minimax.io/v1/text/chatcompletion_v2.
//
// Diferenças de outros providers:
//   - Schema OpenAI-compatible (messages array com role+content)
//   - System prompt vai como primeira mensagem com role="system"
//   - Auth via Bearer token (igual OpenAI)
//   - Default model `MiniMax-M1` (M2.7 ainda não público em 2026-05-20)
type MiniMaxProvider struct {
	apiKey  string
	model   string
	baseURL string
	timeout time.Duration
	client  *http.Client
	limiter *rate.Limiter
}

func NewMiniMax(cfg config.ProviderConfig) *MiniMaxProvider {
	model := cfg.Model
	if model == "" {
		model = "MiniMax-M1"
	}
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = "https://api.minimax.io"
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	return &MiniMaxProvider{
		apiKey:  cfg.APIKey,
		model:   model,
		baseURL: baseURL,
		timeout: timeout,
		client:  &http.Client{Timeout: timeout},
		limiter: newLimiter(cfg.RateLimitRPS),
	}
}

func (m *MiniMaxProvider) Name() string  { return "minimax" }
func (m *MiniMaxProvider) Model() string { return m.model }

type minimaxMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type minimaxRequest struct {
	Model       string           `json:"model"`
	Messages    []minimaxMessage `json:"messages"`
	MaxTokens   int              `json:"max_tokens,omitempty"`
	Temperature float64          `json:"temperature,omitempty"`
	Stream      bool             `json:"stream,omitempty"`
}

type minimaxResponse struct {
	Choices []struct {
		Message minimaxMessage `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
	BaseResp struct {
		StatusCode int    `json:"status_code"`
		StatusMsg  string `json:"status_msg"`
	} `json:"base_resp"`
}

func (m *MiniMaxProvider) Complete(ctx context.Context, prompt Prompt) (Result, error) {
	if m.apiKey == "" {
		return Result{}, errors.New("minimax: API key não configurada")
	}
	if err := waitLimiter(ctx, m.limiter); err != nil {
		return Result{}, fmt.Errorf("minimax: rate limit: %w", err)
	}
	start := time.Now()

	messages := make([]minimaxMessage, 0, 2)
	if prompt.System != "" {
		messages = append(messages, minimaxMessage{Role: "system", Content: prompt.System})
	}
	messages = append(messages, minimaxMessage{Role: "user", Content: prompt.User})

	body := minimaxRequest{
		Model:       m.model,
		Messages:    messages,
		MaxTokens:   prompt.MaxTokens,
		Temperature: prompt.Temperature,
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return Result{}, fmt.Errorf("minimax: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.baseURL+"/v1/text/chatcompletion_v2", bytes.NewReader(raw))
	if err != nil {
		return Result{}, fmt.Errorf("minimax: new req: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+m.apiKey)

	resp, err := m.client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("minimax: http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		msg := string(b)
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return Result{}, &HTTPError{Status: resp.StatusCode, Body: msg, Provider: "minimax"}
	}

	var mr minimaxResponse
	if err := json.NewDecoder(resp.Body).Decode(&mr); err != nil {
		return Result{}, fmt.Errorf("minimax: decode: %w", err)
	}

	// MiniMax sinaliza erro de aplicação via base_resp mesmo com HTTP 200.
	// Erros server-side/transitórios (1000 unknown/"520", 1002 rate-limited, 1027,
	// 1039/1042 rate, etc.) viram *HTTPError 5xx → o Router cai pro fallback
	// (ollama-turbo). Request-bugs (auth, saldo, params inválidos) ficam
	// não-retryable: trocar de provider não resolve.
	if code := mr.BaseResp.StatusCode; code != 0 && code != 200 {
		switch code {
		case 1004, 1008, 2013, 2049: // auth / saldo / params inválidos / key inválida
			return Result{}, fmt.Errorf("minimax: app err %d: %s", code, mr.BaseResp.StatusMsg)
		default:
			return Result{}, &HTTPError{
				Status:   http.StatusBadGateway,
				Body:     fmt.Sprintf("app err %d: %s", code, mr.BaseResp.StatusMsg),
				Provider: "minimax",
			}
		}
	}

	if len(mr.Choices) == 0 {
		return Result{}, errors.New("minimax: resposta vazia (no choices)")
	}

	return Result{
		Content:   stripThinking(mr.Choices[0].Message.Content),
		Provider:  m.Name(),
		Model:     m.model,
		TokensIn:  mr.Usage.PromptTokens,
		TokensOut: mr.Usage.CompletionTokens,
		LatencyMs: int(time.Since(start).Milliseconds()),
	}, nil
}

// stripThinking remove blocos <think>...</think> que modelos reasoning
// (MiniMax M2.7, DeepSeek R1, etc.) emitem antes da resposta final.
// Esses blocos são chain-of-thought interno — não devem entrar na resposta
// ao usuário/judge. Caso o bloco esteja inacabado (LLM truncou no meio do
// raciocínio), descarta apenas o que veio depois do <think> SEM remover
// nada — o restante já é a resposta válida.
func stripThinking(s string) string {
	for {
		i := strings.Index(s, "<think>")
		if i < 0 {
			break
		}
		j := strings.Index(s[i:], "</think>")
		if j < 0 {
			// Bloco aberto sem fechamento — drop tudo depois do <think>
			s = strings.TrimSpace(s[:i])
			break
		}
		s = s[:i] + s[i+j+len("</think>"):]
	}
	return strings.TrimSpace(s)
}
