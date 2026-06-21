package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Stream implementa StreamingProvider via Gemini :streamGenerateContent?alt=sse.
//
// Cada `data: {...}` é uma response parcial contendo candidates[0].content.parts[].text
// incremental (não acumulativo). usageMetadata aparece no último chunk com totais.
//
// Diferente do Anthropic, Gemini não tem event type explícito — só JSONs sequenciais.
// O "fim" é detectado pelo EOF do body.
func (g *GeminiProvider) Stream(ctx context.Context, prompt Prompt) (<-chan StreamChunk, error) {
	if g.apiKey == "" {
		return nil, errors.New("gemini stream: API key não configurada")
	}
	if err := waitLimiter(ctx, g.limiter); err != nil {
		return nil, fmt.Errorf("gemini stream: rate limit: %w", err)
	}
	start := time.Now()

	body := geminiRequest{
		Contents: []geminiContent{
			{Role: "user", Parts: []geminiPart{{Text: prompt.User}}},
		},
		GenerationConfig: geminiGenConfig{
			MaxOutputTokens: prompt.MaxTokens,
			Temperature:     prompt.Temperature,
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
		return nil, fmt.Errorf("gemini stream: marshal: %w", err)
	}

	url := fmt.Sprintf("%s/v1beta/models/%s:streamGenerateContent?alt=sse&key=%s", g.baseURL, g.model, g.apiKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("gemini stream: new req: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gemini stream: http: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		msg := string(b)
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return nil, &HTTPError{Status: resp.StatusCode, Body: msg, Provider: "gemini"}
	}

	out := make(chan StreamChunk, 16)
	go geminiReadSSE(ctx, resp.Body, out, g.Name(), g.model, start)
	return out, nil
}

func geminiReadSSE(ctx context.Context, body io.ReadCloser, out chan<- StreamChunk, providerName, model string, start time.Time) {
	defer close(out)
	defer func() { _ = body.Close() }()

	var tokensIn, tokensOut int
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "" {
			continue
		}

		var evt geminiResponse
		if err := json.Unmarshal([]byte(payload), &evt); err != nil {
			select {
			case out <- StreamChunk{Err: fmt.Errorf("gemini stream: bad json: %w", err), Done: true}:
			case <-ctx.Done():
			}
			return
		}

		// usageMetadata aparece em chunks intermediários (cumulativo) — pega o último valor
		if evt.UsageMetadata.PromptTokenCount > 0 {
			tokensIn = evt.UsageMetadata.PromptTokenCount
		}
		if evt.UsageMetadata.CandidatesTokenCount > 0 {
			tokensOut = evt.UsageMetadata.CandidatesTokenCount
		}

		// Concatena parts.text dos candidates[0]
		if len(evt.Candidates) > 0 {
			var delta string
			for _, p := range evt.Candidates[0].Content.Parts {
				delta += p.Text
			}
			if delta != "" {
				select {
				case out <- StreamChunk{Delta: delta}:
				case <-ctx.Done():
					return
				}
			}
		}
	}

	if err := scanner.Err(); err != nil {
		select {
		case out <- StreamChunk{Err: fmt.Errorf("gemini stream: read: %w", err), Done: true}:
		case <-ctx.Done():
		}
		return
	}

	// EOF natural — emite Done com usage acumulado
	select {
	case out <- StreamChunk{
		Done:      true,
		TokensIn:  tokensIn,
		TokensOut: tokensOut,
		Provider:  providerName,
		Model:     model,
		LatencyMs: int(time.Since(start).Milliseconds()),
	}:
	case <-ctx.Done():
	}
}
