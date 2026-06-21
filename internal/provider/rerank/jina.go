package rerank

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// jinaBrowserUA — UA de browser. O Cloudflare na frente da api.jina.ai bloqueia
// clientes não-browser (Go-http-client) com "error code: 1010" → HTTP 403.
const jinaBrowserUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36"

// JinaReranker chama Jina /v1/rerank (cross-encoder hospedado).
//
// jina-reranker-v3: multilíngue, hospedado → ~0.67s pra 32 docs vs ~20-68s no
// TEI-CPU. Sem necessidade de sub-batch (a API aceita o pool inteiro). Manda
// truncate=true + max_tokens_per_doc (chunks ~400 tok < 2048 → não trunca).
type JinaReranker struct {
	apiKey          string
	baseURL         string
	model           string
	maxTokensPerDoc int
	truncate        bool
	client          *http.Client
}

// NewJina cria o reranker. baseURL ex: "https://api.jina.ai".
// maxTokensPerDoc: teto de tokens/doc (1-8192; 0 = default da API=2048).
// truncate: true → trunca doc longo em vez de errar a request.
func NewJina(apiKey, model, baseURL string, timeout time.Duration, maxTokensPerDoc int, truncate bool) *JinaReranker {
	if baseURL == "" {
		baseURL = "https://api.jina.ai"
	}
	if model == "" {
		model = "jina-reranker-v3"
	}
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	return &JinaReranker{
		apiKey:          apiKey,
		baseURL:         baseURL,
		model:           model,
		maxTokensPerDoc: maxTokensPerDoc,
		truncate:        truncate,
		client:          &http.Client{Timeout: timeout},
	}
}

func (j *JinaReranker) Name() string  { return "jina" }
func (j *JinaReranker) Model() string { return j.model }

// jinaRerankRequest — payload de /v1/rerank (schema RerankerV3Request).
// ATENÇÃO: o reranker usa nomes DIFERENTES do /v1/embeddings — `truncation`
// (não `truncate`) e `max_doc_length` (não `max_tokens_per_doc`). Mandar os
// nomes do embed faz a API v3 rejeitar o corpo → rerank quebra (cai no RRF).
type jinaRerankRequest struct {
	Model           string   `json:"model"`
	Query           string   `json:"query"`
	Documents       []string `json:"documents"`
	TopN            int      `json:"top_n,omitempty"`
	ReturnDocuments bool     `json:"return_documents"`
	MaxDocLength    int      `json:"max_doc_length,omitempty"`
	Truncation      bool     `json:"truncation,omitempty"`
}

type jinaRerankResponse struct {
	Results []struct {
		Index          int     `json:"index"`
		RelevanceScore float64 `json:"relevance_score"`
	} `json:"results"`
	Usage struct {
		TotalTokens int `json:"total_tokens"`
	} `json:"usage"`
}

// Rerank reordena documents conforme query. topK=0 retorna todos reordenados.
func (j *JinaReranker) Rerank(ctx context.Context, query string, documents []string, topK int) ([]Result, error) {
	if len(documents) == 0 {
		return nil, nil
	}
	if j.apiKey == "" {
		return nil, errors.New("jina rerank: JINA_API_KEY não configurada")
	}

	topN := topK
	if topN <= 0 || topN > len(documents) {
		topN = len(documents)
	}

	raw, err := json.Marshal(jinaRerankRequest{
		Model:           j.model,
		Query:           query,
		Documents:       documents,
		TopN:            topN,
		ReturnDocuments: false,
		MaxDocLength:    j.maxTokensPerDoc,
		Truncation:      j.truncate,
	})
	if err != nil {
		return nil, fmt.Errorf("jina rerank: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, j.baseURL+"/v1/rerank", bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("jina rerank: new req: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+j.apiKey)
	req.Header.Set("User-Agent", jinaBrowserUA)

	resp, err := j.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jina rerank: http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		msg := string(b)
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return nil, fmt.Errorf("jina rerank: http %d: %s", resp.StatusCode, msg)
	}

	var rr jinaRerankResponse
	if err := json.NewDecoder(resp.Body).Decode(&rr); err != nil {
		return nil, fmt.Errorf("jina rerank: decode: %w", err)
	}

	out := make([]Result, 0, len(rr.Results))
	for _, it := range rr.Results {
		if it.Index < 0 || it.Index >= len(documents) {
			continue
		}
		out = append(out, Result{Index: it.Index, Score: it.RelevanceScore})
	}
	return out, nil
}
