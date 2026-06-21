// Package embed abstrai providers de embedding (Jina AI hospedado).
//
// Filosofia:
//   - Uma interface única — fácil trocar/encadear providers
//   - Batch by default — chamadas single são wrappers em batch de 1
//   - Rate limiting integrado (client-side) — evita HTTP 429 do provider
//   - Sem cache aqui — cache é responsabilidade de quem chama (Day 9+)
package embed

import (
	"context"
	"errors"
	"fmt"

	"github.com/nexusyn/engine/internal/config"
)

// Provider é a interface implementada por cada vendor de embedding.
//
// Contract:
//   - Embed retorna 1 vetor por texto de entrada, na MESMA ordem
//   - len(result) == len(texts) sempre (ou retorna erro)
//   - texts vazio → []Vector{}, nil (no-op)
//   - Cada vetor tem Dim() == EmbedDims (configurado por provider)
//   - Rate limiting é responsabilidade do provider (interno)
type Provider interface {
	// Embed gera embeddings para múltiplos textos numa única chamada (batch).
	// Provider implementa rate limiting + retry internally.
	Embed(ctx context.Context, texts []string, inputType InputType) ([][]float32, error)

	// Name retorna identificador do provider (ex: "jina").
	Name() string

	// Model retorna o modelo específico em uso.
	Model() string

	// Dim retorna a dimensionalidade dos vetores produzidos.
	Dim() int
}

// InputType discrimina embedding pra documento (indexação) vs query (busca).
// Jina distingue (task) — produz melhor recall em retrieval.
type InputType string

const (
	InputTypeDocument InputType = "document"
	InputTypeQuery    InputType = "query"
)

// ErrEmptyText é retornado quando input contém string vazia.
// Provider pode optar por filtrar vazio antes ou retornar este erro.
var ErrEmptyText = errors.New("embed: input text vazio")

// BuildForKey constrói um embed provider com override de provider/model/key/baseURL
// (vazio = mantém o default do cfg). Usado pelo teste de conexão da config global.
func BuildForKey(base config.EmbedConfig, provider, model, apiKey, baseURL string) (Provider, error) {
	if provider != "" {
		base.Provider = provider
	}
	switch base.Provider {
	case "jina", "":
		if apiKey != "" {
			base.Jina.APIKey = apiKey
		}
		if model != "" {
			base.Jina.EmbedModel = model
		}
		if baseURL != "" {
			base.Jina.BaseURL = baseURL
		}
	}
	return Factory(base)
}

// Factory cria o Provider configurado via env.
// Trocar provider em prod = mudar EMBED_PROVIDER, sem rebuild.
func Factory(cfg config.EmbedConfig) (Provider, error) {
	switch cfg.Provider {
	case "jina", "":
		// Jina AI hospedado (jina-embeddings-v5-text-small) — sem gargalo de CPU.
		return NewJina(cfg.Jina, cfg.RateLimitRPS), nil
	default:
		return nil, fmt.Errorf("embed: provider desconhecido: %s", cfg.Provider)
	}
}
