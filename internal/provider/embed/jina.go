package embed

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

// browserUA é o User-Agent enviado em toda chamada à Jina. O Cloudflare na
// frente da api.jina.ai bloqueia clientes não-browser (Go-http-client,
// python-urllib) com "error code: 1010" → HTTP 403. curl/browser passam.
const browserUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36"

// JinaProvider implementa Provider via Jina AI API (api.jina.ai/v1/embeddings).
//
// jina-embeddings-v5-text-small: multilíngue, contexto 32K tok, task-specific
// (LoRA), Matryoshka; 1024 dims nativos (compat com schema). Hospedado → sem o
// gargalo de CPU do TEI local (embed de 32 chunks ~1.6s vs ~33-50s). Aceita
// late_chunking (não usado hoje — chunkamos antes do embed).
type JinaProvider struct {
	apiKey  string
	baseURL string
	model   string
	dim     int
	timeout time.Duration

	client  *http.Client
	limiter *rate.Limiter
}

// NewJina cria o provider. rateLimitRPS=0 desabilita throttling client-side.
func NewJina(cfg config.JinaConfig, rateLimitRPS int) *JinaProvider {
	var limiter *rate.Limiter
	if rateLimitRPS > 0 {
		limiter = rate.NewLimiter(rate.Limit(rateLimitRPS), rateLimitRPS*2)
	}
	return &JinaProvider{
		apiKey:  cfg.APIKey,
		baseURL: cfg.BaseURL,
		model:   cfg.EmbedModel,
		dim:     cfg.EmbedDims,
		timeout: cfg.Timeout,
		client:  &http.Client{Timeout: cfg.Timeout},
		limiter: limiter,
	}
}

func (j *JinaProvider) Name() string  { return "jina" }
func (j *JinaProvider) Model() string { return j.model }
func (j *JinaProvider) Dim() int      { return j.dim }

// jinaEmbedRequest — payload de /v1/embeddings.
// task: jina-embeddings-v3 usa LoRA específica por tarefa (melhor recall).
type jinaEmbedRequest struct {
	Model      string   `json:"model"`
	Input      []string `json:"input"`
	Task       string   `json:"task,omitempty"`
	Dimensions int      `json:"dimensions,omitempty"`
	// Truncate: trunca input que excede o contexto (32K tok no v5) em vez de
	// retornar erro — rede de segurança contra chunk gigante quebrar o ingest.
	Truncate bool `json:"truncate"`
}

type jinaEmbedResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
	Model string `json:"model"`
	Usage struct {
		TotalTokens int `json:"total_tokens"`
	} `json:"usage"`
}

// jinaTask mapeia o InputType pro task LoRA da v3.
func jinaTask(t InputType) string {
	if t == InputTypeQuery {
		return "retrieval.query"
	}
	return "retrieval.passage"
}

// Embed chama Jina /v1/embeddings com batch de textos. 1 vetor por texto, mesma ordem.
func (j *JinaProvider) Embed(ctx context.Context, texts []string, inputType InputType) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	if j.apiKey == "" {
		return nil, errors.New("jina: JINA_API_KEY não configurada")
	}

	if j.limiter != nil {
		if err := j.limiter.Wait(ctx); err != nil {
			return nil, fmt.Errorf("jina: rate limit wait: %w", err)
		}
	}

	raw, err := json.Marshal(jinaEmbedRequest{
		Model:      j.model,
		Input:      texts,
		Task:       jinaTask(inputType),
		Dimensions: j.dim,
		Truncate:   true,
	})
	if err != nil {
		return nil, fmt.Errorf("jina: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, j.baseURL+"/v1/embeddings", bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("jina: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+j.apiKey)
	req.Header.Set("User-Agent", browserUA)

	resp, err := j.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jina: http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		msg := string(respBody)
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return nil, fmt.Errorf("jina: http %d: %s", resp.StatusCode, msg)
	}

	var er jinaEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		return nil, fmt.Errorf("jina: decode response: %w", err)
	}

	out := make([][]float32, len(texts))
	for _, item := range er.Data {
		if item.Index < 0 || item.Index >= len(out) {
			return nil, fmt.Errorf("jina: index out of range: %d", item.Index)
		}
		out[item.Index] = item.Embedding
	}
	for i, v := range out {
		if v == nil {
			return nil, fmt.Errorf("jina: missing embedding for index %d", i)
		}
	}

	return out, nil
}
