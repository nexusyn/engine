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

// OpenRouterProvider fala com o OpenRouter (openrouter.ai) via a API
// OpenAI-compatible em https://openrouter.ai/api/v1/chat/completions.
//
// Gateway pra centenas de modelos (Gemini, Claude, GPT, etc.) com UMA key —
// útil quando o billing direto do vendor cai (ex: Google sem faturamento →
// usar google/gemini-3.5-flash via OpenRouter). Reusa os tipos oai* (formato
// idêntico ao OllamaTurbo). Config via OPENROUTER_* (API_KEY/MODEL/BASE_URL).
type OpenRouterProvider struct {
	apiKey  string
	model   string
	baseURL string
	client  *http.Client
	limiter *rate.Limiter
}

func NewOpenRouter(cfg config.ProviderConfig) *OpenRouterProvider {
	model := cfg.Model
	if model == "" {
		model = "google/gemini-3.5-flash"
	}
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = "https://openrouter.ai/api"
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	return &OpenRouterProvider{
		apiKey:  cfg.APIKey,
		model:   model,
		baseURL: baseURL,
		client:  &http.Client{Timeout: timeout},
		limiter: newLimiter(cfg.RateLimitRPS),
	}
}

func (p *OpenRouterProvider) Name() string  { return "openrouter" }
func (p *OpenRouterProvider) Model() string { return p.model }

// Complete envia chat completion ao OpenRouter (OpenAI-compatible).
func (p *OpenRouterProvider) Complete(ctx context.Context, prompt Prompt) (Result, error) {
	start := time.Now()

	if p.apiKey == "" {
		return Result{}, errors.New("openrouter: API key não configurada")
	}
	if p.limiter != nil {
		if err := p.limiter.Wait(ctx); err != nil {
			return Result{}, fmt.Errorf("openrouter: rate limit: %w", err)
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
		return Result{}, fmt.Errorf("openrouter: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return Result{}, fmt.Errorf("openrouter: new req: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	// Headers opcionais de atribuição do OpenRouter (não obrigatórios).
	req.Header.Set("X-Title", "nexusyn")

	resp, err := p.client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("openrouter: http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		var errBody oaiErrorBody
		_ = json.Unmarshal(b, &errBody)
		msg := errBody.Error.Message
		if msg == "" {
			msg = string(b)
			if len(msg) > 300 {
				msg = msg[:300]
			}
		}
		return Result{}, &HTTPError{Status: resp.StatusCode, Body: msg, Provider: "openrouter"}
	}

	var tr oaiResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return Result{}, fmt.Errorf("openrouter: decode: %w", err)
	}
	if len(tr.Choices) == 0 {
		return Result{}, errors.New("openrouter: resposta vazia (no choices)")
	}

	return Result{
		Content:   stripThinking(tr.Choices[0].Message.Content),
		Provider:  p.Name(),
		Model:     p.model,
		TokensIn:  tr.Usage.PromptTokens,
		TokensOut: tr.Usage.CompletionTokens,
		LatencyMs: int(time.Since(start).Milliseconds()),
	}, nil
}
