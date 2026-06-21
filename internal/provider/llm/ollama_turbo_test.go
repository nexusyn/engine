package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexusyn/engine/internal/config"
)

func TestOllamaTurbo_Complete_HappyPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/chat/completions", r.URL.Path)
		assert.Equal(t, "Bearer testkey", r.Header.Get("Authorization"))
		var req oaiRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		assert.Equal(t, "gpt-oss:120b", req.Model)
		require.Len(t, req.Messages, 2)
		assert.Equal(t, "system", req.Messages[0].Role)
		assert.Equal(t, "user", req.Messages[1].Role)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(oaiResponse{
			Choices: []struct {
				Message      oaiMessage `json:"message"`
				FinishReason string     `json:"finish_reason"`
			}{
				{Message: oaiMessage{Role: "assistant", Content: "olá, mundo"}, FinishReason: "stop"},
			},
			Usage: struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			}{PromptTokens: 12, CompletionTokens: 5, TotalTokens: 17},
		})
	}))
	defer server.Close()

	p := NewOllamaTurbo(config.ProviderConfig{APIKey: "testkey", BaseURL: server.URL, Timeout: 5 * time.Second})
	assert.Equal(t, "ollama-turbo", p.Name())
	assert.Equal(t, "gpt-oss:120b", p.Model())

	res, err := p.Complete(context.Background(), Prompt{
		System:    "sys",
		User:      "hi",
		MaxTokens: 100,
	})
	require.NoError(t, err)
	assert.Equal(t, "olá, mundo", res.Content)
	assert.Equal(t, 12, res.TokensIn)
	assert.Equal(t, 5, res.TokensOut)
	assert.Equal(t, "ollama-turbo", res.Provider)
}

func TestOllamaTurbo_Complete_JSONMode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req oaiRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		require.NotNil(t, req.ResponseFormat, "ResponseFormat deve estar setado em JSONMode")
		assert.Equal(t, "json_object", req.ResponseFormat.Type)

		_ = json.NewEncoder(w).Encode(oaiResponse{
			Choices: []struct {
				Message      oaiMessage `json:"message"`
				FinishReason string     `json:"finish_reason"`
			}{{Message: oaiMessage{Content: `{"entities":[]}`}, FinishReason: "stop"}},
		})
	}))
	defer server.Close()

	p := NewOllamaTurbo(config.ProviderConfig{APIKey: "k", BaseURL: server.URL})
	res, err := p.Complete(context.Background(), Prompt{User: "extract", JSONMode: true})
	require.NoError(t, err)
	assert.Equal(t, `{"entities":[]}`, res.Content)
}

func TestOllamaTurbo_Complete_StripsThinking(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(oaiResponse{
			Choices: []struct {
				Message      oaiMessage `json:"message"`
				FinishReason string     `json:"finish_reason"`
			}{{Message: oaiMessage{Content: "<think>step</think>Final answer"}, FinishReason: "stop"}},
		})
	}))
	defer server.Close()

	p := NewOllamaTurbo(config.ProviderConfig{APIKey: "k", BaseURL: server.URL})
	res, err := p.Complete(context.Background(), Prompt{User: "x"})
	require.NoError(t, err)
	assert.Equal(t, "Final answer", res.Content)
}

func TestOllamaTurbo_Complete_HTTPError_Typed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(oaiErrorBody{
			Error: struct {
				Message string `json:"message"`
				Type    string `json:"type"`
			}{Message: "rate limit", Type: "rate_limit_error"},
		})
	}))
	defer server.Close()

	p := NewOllamaTurbo(config.ProviderConfig{APIKey: "k", BaseURL: server.URL})
	_, err := p.Complete(context.Background(), Prompt{User: "x"})
	require.Error(t, err)
	var herr *HTTPError
	require.True(t, errors.As(err, &herr))
	assert.Equal(t, 429, herr.Status)
	assert.Contains(t, herr.Body, "rate limit")
	assert.Equal(t, "ollama-turbo", herr.Provider)
}

func TestOllamaTurbo_Complete_MissingAPIKey_Errors(t *testing.T) {
	p := NewOllamaTurbo(config.ProviderConfig{})
	_, err := p.Complete(context.Background(), Prompt{User: "x"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "API key não configurada")
}
