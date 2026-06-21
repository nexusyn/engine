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

// AnthropicProvider chama api.anthropic.com/v1/messages.
//
// Diferenças de outros providers:
//   - System prompt vai em campo `system` separado (não dentro de messages)
//   - Auth via header `x-api-key` (não Authorization Bearer)
//   - Versão da API em header `anthropic-version` (obrigatório)
//   - Usage tem `input_tokens` + `output_tokens` direto na response
type AnthropicProvider struct {
	apiKey     string
	model      string
	baseURL    string
	apiVersion string
	timeout    time.Duration
	client     *http.Client
	limiter    *rate.Limiter
}

const anthropicDefaultVersion = "2023-06-01"

func NewAnthropic(cfg config.ProviderConfig) *AnthropicProvider {
	model := cfg.Model
	if model == "" {
		model = "claude-haiku-4-5"
	}
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	return &AnthropicProvider{
		apiKey:     cfg.APIKey,
		model:      model,
		baseURL:    baseURL,
		apiVersion: anthropicDefaultVersion,
		timeout:    timeout,
		client:     &http.Client{Timeout: timeout},
		limiter:    newLimiter(cfg.RateLimitRPS),
	}
}

func (a *AnthropicProvider) Name() string  { return "anthropic" }
func (a *AnthropicProvider) Model() string { return a.model }

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicRequest struct {
	Model       string             `json:"model"`
	System      string             `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	MaxTokens   int                `json:"max_tokens"`
	Temperature float64            `json:"temperature,omitempty"`
}

type anthropicResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func (a *AnthropicProvider) Complete(ctx context.Context, prompt Prompt) (Result, error) {
	if a.apiKey == "" {
		return Result{}, errors.New("anthropic: API key não configurada")
	}
	if err := waitLimiter(ctx, a.limiter); err != nil {
		return Result{}, fmt.Errorf("anthropic: rate limit: %w", err)
	}
	start := time.Now()

	maxTokens := prompt.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 1024 // Anthropic exige max_tokens > 0
	}

	body := anthropicRequest{
		Model:       a.model,
		System:      prompt.System,
		Messages:    []anthropicMessage{{Role: "user", Content: prompt.User}},
		MaxTokens:   maxTokens,
		Temperature: prompt.Temperature,
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return Result{}, fmt.Errorf("anthropic: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/v1/messages", bytes.NewReader(raw))
	if err != nil {
		return Result{}, fmt.Errorf("anthropic: new req: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", a.apiKey)
	req.Header.Set("anthropic-version", a.apiVersion)

	resp, err := a.client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("anthropic: http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		msg := string(b)
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return Result{}, &HTTPError{Status: resp.StatusCode, Body: msg, Provider: "anthropic"}
	}

	var ar anthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&ar); err != nil {
		return Result{}, fmt.Errorf("anthropic: decode: %w", err)
	}

	if len(ar.Content) == 0 {
		return Result{}, errors.New("anthropic: resposta vazia (no content)")
	}

	var content string
	for _, c := range ar.Content {
		if c.Type == "text" {
			content += c.Text
		}
	}

	return Result{
		Content:   content,
		Provider:  a.Name(),
		Model:     a.model,
		TokensIn:  ar.Usage.InputTokens,
		TokensOut: ar.Usage.OutputTokens,
		LatencyMs: int(time.Since(start).Milliseconds()),
	}, nil
}
