package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"golang.org/x/time/rate"

	"github.com/nexusyn/engine/internal/config"
)

// OllamaTurboProvider fala com o Ollama Cloud (Ollama Turbo) via a API
// OpenAI-compatible em https://ollama.com/v1/chat/completions.
//
// ATENÇÃO: NÃO é together.xyz. O nome legado "together" foi mantido só como
// alias de back-compat no factory (build) e no envPrefix; o provider real é
// o Ollama Cloud Turbo. Config via OLLAMA_* (API_KEY/MODEL/BASE_URL/TIMEOUT).
//
// Modelo em prod: gpt-oss:120b (OLLAMA_MODEL). Endpoint OpenAI-compat aceita
// response_format={"type":"json_object"} pra JSON mode (usado no extractor).
type OllamaTurboProvider struct {
	apiKey  string
	model   string
	baseURL string
	timeout time.Duration
	client  *http.Client
	limiter *rate.Limiter
}

func NewOllamaTurbo(cfg config.ProviderConfig) *OllamaTurboProvider {
	model := cfg.Model
	if model == "" {
		model = "gpt-oss:120b"
	}
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = "https://ollama.com"
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	return &OllamaTurboProvider{
		apiKey:  cfg.APIKey,
		model:   model,
		baseURL: baseURL,
		timeout: timeout,
		client:  &http.Client{Timeout: timeout},
		limiter: newLimiter(cfg.RateLimitRPS),
	}
}

func (p *OllamaTurboProvider) Name() string  { return "ollama-turbo" }
func (p *OllamaTurboProvider) Model() string { return p.model }

type oaiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type oaiResponseFormat struct {
	Type string `json:"type"` // "json_object" para JSON mode
}

type oaiRequest struct {
	Model          string             `json:"model"`
	Messages       []oaiMessage       `json:"messages"`
	MaxTokens      int                `json:"max_tokens,omitempty"`
	Temperature    float64            `json:"temperature,omitempty"`
	ResponseFormat *oaiResponseFormat `json:"response_format,omitempty"`
}

type oaiResponse struct {
	Choices []struct {
		Message      oaiMessage `json:"message"`
		FinishReason string     `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

type oaiErrorBody struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// Complete envia chat completion ao Ollama Cloud Turbo (OpenAI-compatible).
//
// JSONMode: ativa response_format={"type":"json_object"}. Também garante que
// o system prompt inclui "JSON only" pra reforço.
func (p *OllamaTurboProvider) Complete(ctx context.Context, prompt Prompt) (Result, error) {
	start := time.Now()

	if p.apiKey == "" {
		return Result{}, errors.New("ollama-turbo: API key não configurada")
	}
	if p.limiter != nil {
		if err := p.limiter.Wait(ctx); err != nil {
			return Result{}, fmt.Errorf("ollama-turbo: rate limit: %w", err)
		}
	}

	messages := make([]oaiMessage, 0, 2)
	if prompt.System != "" {
		messages = append(messages, oaiMessage{Role: "system", Content: prompt.System})
	}
	messages = append(messages, oaiMessage{Role: "user", Content: prompt.User})

	body := oaiRequest{
		Model:       p.model,
		Messages:    messages,
		MaxTokens:   prompt.MaxTokens,
		Temperature: prompt.Temperature,
	}
	if prompt.JSONMode {
		body.ResponseFormat = &oaiResponseFormat{Type: "json_object"}
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return Result{}, fmt.Errorf("ollama-turbo: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return Result{}, fmt.Errorf("ollama-turbo: new req: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("ollama-turbo: http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		// Tenta parsear erro estruturado, senão usa body raw
		var errBody oaiErrorBody
		_ = json.Unmarshal(b, &errBody)
		msg := errBody.Error.Message
		if msg == "" {
			msg = string(b)
			if len(msg) > 300 {
				msg = msg[:300]
			}
		}
		return Result{}, &HTTPError{Status: resp.StatusCode, Body: msg, Provider: "ollama-turbo"}
	}

	var tr oaiResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return Result{}, fmt.Errorf("ollama-turbo: decode: %w", err)
	}

	if len(tr.Choices) == 0 {
		return Result{}, errors.New("ollama-turbo: resposta vazia (no choices)")
	}

	// stripThinking (de minimax.go): gpt-oss e modelos com <think> emitem
	// reasoning antes da resposta — removido aqui.
	return Result{
		Content:   stripThinking(tr.Choices[0].Message.Content),
		Provider:  p.Name(),
		Model:     p.model,
		TokensIn:  tr.Usage.PromptTokens,
		TokensOut: tr.Usage.CompletionTokens,
		LatencyMs: int(time.Since(start).Milliseconds()),
	}, nil
}
