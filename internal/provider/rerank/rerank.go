// Package rerank abstrai re-rankers de busca (Jina reranker hospedado).
//
// Filosofia:
//   - Rerank é opcional — search/RRF funciona standalone
//   - Aplicado pós-RRF em top-K candidates → top-N final
//   - Single vendor (Jina) por default — mesma key do embed (single source)
package rerank

import (
	"context"
	"fmt"

	"github.com/nexusyn/engine/internal/config"
)

// Provider é a interface implementada por cada vendor de rerank.
type Provider interface {
	// Rerank reordena documents conforme relevância à query.
	// Retorna lista de Result na ordem ranqueada (top-N pelo provider).
	Rerank(ctx context.Context, query string, documents []string, topK int) ([]Result, error)
	Name() string
	Model() string
}

// Result é um document reordenado com seu score relativo.
type Result struct {
	Index int     // índice na lista original
	Score float64 // relevance score do vendor (0-1 tipicamente)
}

// BuildForKey constrói um reranker com override de driver/model/key/baseURL
// (vazio = mantém o default). O Jina lê a key do embedCfg (mesma conta).
// Usado pelo teste de conexão da config global.
func BuildForKey(base config.RerankConfig, embedBase config.EmbedConfig, provider, model, apiKey, baseURL string) (Provider, error) {
	if provider != "" {
		base.Driver = provider
	}
	switch base.Driver {
	case "jina", "":
		if apiKey != "" {
			embedBase.Jina.APIKey = apiKey
		}
		if model != "" {
			base.JinaModel = model
		}
		if baseURL != "" {
			embedBase.Jina.BaseURL = baseURL
		}
	}
	return Factory(base, embedBase)
}

// Factory cria o Provider configurado via env (RERANKER=jina|none).
// Recebe a EmbedConfig pra reusar as creds do Jina (a MESMA conta/key serve
// embed + rerank).
func Factory(cfg config.RerankConfig, embedCfg config.EmbedConfig) (Provider, error) {
	switch cfg.Driver {
	case "jina", "":
		// Jina AI hospedado — mesma key do embed Jina, sem gargalo de CPU.
		return NewJina(embedCfg.Jina.APIKey, cfg.JinaModel, embedCfg.Jina.BaseURL, cfg.JinaTimeout, cfg.JinaMaxTokensPerDoc, cfg.JinaTruncate), nil
	case "none":
		return nil, nil
	default:
		return nil, fmt.Errorf("rerank: driver desconhecido: %s", cfg.Driver)
	}
}
