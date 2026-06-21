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

// GeminiProvider chama Google Gemini via generativelanguage.googleapis.com.
//
// Diferenças de outros providers:
//   - System prompt vai em `systemInstruction` (campo separado fora de contents)
//   - Roles: 'user' e 'model' (não 'assistant')
//   - API key na query string `?key=...`, não em header
type GeminiProvider struct {
	apiKey  string
	model   string
	baseURL string
	timeout time.Duration
	client  *http.Client
	limiter *rate.Limiter // nil = sem rate limit
}

func NewGemini(cfg config.ProviderConfig) *GeminiProvider {
	model := cfg.Model
	if model == "" {
		model = "gemini-2.5-flash"
	}
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = "https://generativelanguage.googleapis.com"
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	return &GeminiProvider{
		apiKey:  cfg.APIKey,
		model:   model,
		baseURL: baseURL,
		timeout: timeout,
		client:  &http.Client{Timeout: timeout},
		limiter: newLimiter(cfg.RateLimitRPS),
	}
}

func (g *GeminiProvider) Name() string  { return "gemini" }
func (g *GeminiProvider) Model() string { return g.model }

type geminiPart struct {
	Text    string `json:"text"`
	Thought bool   `json:"thought,omitempty"` // Gemini 3.x: parts de raciocínio
}

type geminiThinkingConfig struct {
	ThinkingBudget int `json:"thinkingBudget"` // 0 = thinking off (saída direta)
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiRequest struct {
	SystemInstruction *geminiContent  `json:"systemInstruction,omitempty"`
	Contents          []geminiContent `json:"contents"`
	GenerationConfig  geminiGenConfig `json:"generationConfig"`
}

type geminiGenConfig struct {
	MaxOutputTokens  int                   `json:"maxOutputTokens,omitempty"`
	Temperature      float64               `json:"temperature,omitempty"`
	ResponseMimeType string                `json:"responseMimeType,omitempty"`
	ThinkingConfig   *geminiThinkingConfig `json:"thinkingConfig,omitempty"`
}

type geminiResponse struct {
	Candidates []struct {
		Content geminiContent `json:"content"`
	} `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
	} `json:"usageMetadata"`
}

func (g *GeminiProvider) Complete(ctx context.Context, prompt Prompt) (Result, error) {
	if g.apiKey == "" {
		return Result{}, errors.New("gemini: API key não configurada")
	}
	if err := waitLimiter(ctx, g.limiter); err != nil {
		return Result{}, fmt.Errorf("gemini: rate limit: %w", err)
	}
	start := time.Now()

	// thinkingBudget default 0 = resposta direta (evita truncar/vazar scratchpad
	// nos 3.x flash). Count/aggregation queries passam ThinkingBudget>0 pra
	// enumerar antes de responder; aí garantimos maxOutputTokens com folga
	// (thinking + resposta) pra não truncar.
	maxOut := prompt.MaxTokens
	if prompt.ThinkingBudget > 0 && maxOut < prompt.ThinkingBudget+2048 {
		maxOut = prompt.ThinkingBudget + 2048
	}
	body := geminiRequest{
		Contents: []geminiContent{
			{Role: "user", Parts: []geminiPart{{Text: prompt.User}}},
		},
		GenerationConfig: geminiGenConfig{
			MaxOutputTokens: maxOut,
			Temperature:     prompt.Temperature,
			ThinkingConfig:  &geminiThinkingConfig{ThinkingBudget: prompt.ThinkingBudget},
		},
	}
	if prompt.JSONMode {
		body.GenerationConfig.ResponseMimeType = "application/json"
	}
	if prompt.System != "" {
		body.SystemInstruction = &geminiContent{Parts: []geminiPart{{Text: prompt.System}}}
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return Result{}, fmt.Errorf("gemini: marshal: %w", err)
	}

	url := fmt.Sprintf("%s/v1beta/models/%s:generateContent?key=%s", g.baseURL, g.model, g.apiKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return Result{}, fmt.Errorf("gemini: new req: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("gemini: http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		msg := string(b)
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return Result{}, &HTTPError{Status: resp.StatusCode, Body: msg, Provider: "gemini"}
	}

	var gr geminiResponse
	if err := json.NewDecoder(resp.Body).Decode(&gr); err != nil {
		return Result{}, fmt.Errorf("gemini: decode: %w", err)
	}

	if len(gr.Candidates) == 0 || len(gr.Candidates[0].Content.Parts) == 0 {
		return Result{}, errors.New("gemini: resposta vazia (no candidates)")
	}

	var content string
	for _, p := range gr.Candidates[0].Content.Parts {
		if p.Thought {
			continue // ignora parts de raciocínio (Gemini 3.x)
		}
		content += p.Text
	}

	return Result{
		Content:   content,
		Provider:  g.Name(),
		Model:     g.model,
		TokensIn:  gr.UsageMetadata.PromptTokenCount,
		TokensOut: gr.UsageMetadata.CandidatesTokenCount,
		LatencyMs: int(time.Since(start).Milliseconds()),
	}, nil
}
