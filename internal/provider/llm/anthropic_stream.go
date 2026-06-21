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

// Stream implementa StreamingProvider via Anthropic Messages API com stream:true.
//
// SSE events processados:
//   - message_start: chunk inicial com usage (input_tokens) — só preenche TokensIn
//   - content_block_delta: delta.text → StreamChunk{Delta: ...}
//   - message_delta: usage.output_tokens (acumulativo)
//   - message_stop: encerra o stream com Done=true
//   - ping: ignorado
//   - error: emite chunk.Err
func (a *AnthropicProvider) Stream(ctx context.Context, prompt Prompt) (<-chan StreamChunk, error) {
	if a.apiKey == "" {
		return nil, errors.New("anthropic stream: API key não configurada")
	}
	if err := waitLimiter(ctx, a.limiter); err != nil {
		return nil, fmt.Errorf("anthropic stream: rate limit: %w", err)
	}
	start := time.Now()

	maxTokens := prompt.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 1024
	}

	body := struct {
		anthropicRequest
		Stream bool `json:"stream"`
	}{
		anthropicRequest: anthropicRequest{
			Model:       a.model,
			System:      prompt.System,
			Messages:    []anthropicMessage{{Role: "user", Content: prompt.User}},
			MaxTokens:   maxTokens,
			Temperature: prompt.Temperature,
		},
		Stream: true,
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("anthropic stream: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/v1/messages", bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("anthropic stream: new req: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("x-api-key", a.apiKey)
	req.Header.Set("anthropic-version", a.apiVersion)

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("anthropic stream: http: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		msg := string(b)
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return nil, &HTTPError{Status: resp.StatusCode, Body: msg, Provider: "anthropic"}
	}

	out := make(chan StreamChunk, 16)
	go anthropicReadSSE(ctx, resp.Body, out, a.Name(), a.model, start)
	return out, nil
}

// anthropicReadSSE drena o response body SSE e empurra chunks no canal.
// Fecha o canal e o body ao final (sucesso, erro ou ctx canceled).
func anthropicReadSSE(ctx context.Context, body io.ReadCloser, out chan<- StreamChunk, providerName, model string, start time.Time) {
	defer close(out)
	defer func() { _ = body.Close() }()

	var tokensIn, tokensOut int
	scanner := bufio.NewScanner(body)
	// SSE buffers podem ser maiores que default scanner buffer (64KB)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue // ignora linhas em branco / event: prefixes
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "" || payload == "[DONE]" {
			continue
		}

		var evt struct {
			Type  string `json:"type"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
			Message struct {
				Usage struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			} `json:"message"`
			Usage struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(payload), &evt); err != nil {
			select {
			case out <- StreamChunk{Err: fmt.Errorf("anthropic stream: bad json: %w", err), Done: true}:
			case <-ctx.Done():
			}
			return
		}

		switch evt.Type {
		case "message_start":
			tokensIn = evt.Message.Usage.InputTokens
		case "content_block_delta":
			if evt.Delta.Text != "" {
				select {
				case out <- StreamChunk{Delta: evt.Delta.Text}:
				case <-ctx.Done():
					return
				}
			}
		case "message_delta":
			if evt.Usage.OutputTokens > 0 {
				tokensOut = evt.Usage.OutputTokens
			}
		case "message_stop":
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
			return
		case "error":
			select {
			case out <- StreamChunk{Err: fmt.Errorf("anthropic stream: %s", evt.Error.Message), Done: true}:
			case <-ctx.Done():
			}
			return
		}
	}

	if err := scanner.Err(); err != nil {
		select {
		case out <- StreamChunk{Err: fmt.Errorf("anthropic stream: read: %w", err), Done: true}:
		case <-ctx.Done():
		}
	}
}
