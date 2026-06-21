// Package llm abstrai providers de LLM (Gemini, Anthropic, MiniMax).
//
// Filosofia:
//   - Interface mínima — Complete (one-shot) hoje, Stream (SSE) Day 13
//   - Provider configurado via cfg.LLM.Primary, fallback chain via cfg.LLM.Fallbacks
//   - Router (Day 12) tenta primary → fallbacks na ordem; HTTPError 429/5xx é
//     retryable, 4xx (request bug) aborta sem fallback
package llm

import (
	"context"
	"fmt"

	"github.com/nexusyn/engine/internal/config"
)

// Provider é a interface implementada por cada vendor LLM.
type Provider interface {
	// Complete envia prompt e retorna resposta one-shot.
	Complete(ctx context.Context, prompt Prompt) (Result, error)
	Name() string
	Model() string
}

// Prompt é a entrada unificada (independente de vendor).
type Prompt struct {
	System      string
	User        string
	MaxTokens   int     // 0 = default do provider
	Temperature float64 // 0 = default

	// JSONMode pede ao provider pra retornar JSON strict.
	// Gemini: seta responseMimeType=application/json (modo nativo).
	// Anthropic/MiniMax: ignorado (não têm modo nativo equivalente; system
	// prompt já força JSON via instrução textual).
	JSONMode bool

	// ThinkingBudget (tokens) liga raciocínio interno no provider Gemini.
	// 0 = thinking off (resposta direta — default). >0 = enumera antes de
	// responder (usado em count/aggregation queries). Outros providers ignoram.
	ThinkingBudget int
}

// Result é a resposta do LLM.
type Result struct {
	Content   string
	Provider  string
	Model     string
	TokensIn  int
	TokensOut int
	LatencyMs int
}

// Factory cria um Router (Provider composto) com primary + fallbacks
// configurados. Se nenhum provider tiver API key, retorna erro.
//
// Comportamento:
//   - Tenta construir primary; se key ausente, pula
//   - Para cada fallback, mesma regra: pula se key ausente
//   - Se NENHUM ficar disponível, retorna erro
//   - Router lida graciosamente quando só 1 provider está disponível
//     (sem fallback chain, apenas direct call)
func Factory(cfg config.LLMConfig) (Provider, error) {
	var primary Provider
	var fallbacks []Provider

	if p, err := build(cfg.Primary, cfg); err == nil && p != nil {
		primary = p
	}
	for _, name := range cfg.Fallbacks {
		if name == cfg.Primary {
			continue // evita duplicar primary na chain
		}
		if p, err := build(name, cfg); err == nil && p != nil {
			fallbacks = append(fallbacks, p)
		}
	}

	if primary == nil && len(fallbacks) == 0 {
		return nil, fmt.Errorf("llm: nenhum provider configurado (primary=%q, fallbacks=%v)", cfg.Primary, cfg.Fallbacks)
	}

	return NewRouter(primary, fallbacks...), nil
}

// BuildFor constrói UM provider específico (provider + model override) usando
// as keys já configuradas em cfg. Usado pela resolução per-org
// (org_model_config). model vazio mantém o default do provider. Retorna erro
// se o provider não tiver key configurada.
func BuildFor(cfg config.LLMConfig, provider, model string) (Provider, error) {
	if model != "" {
		switch provider {
		case "gemini":
			cfg.Gemini.Model = model
		case "anthropic":
			cfg.Anthropic.Model = model
		case "minimax":
			cfg.MiniMax.Model = model
		case "ollama-turbo", "ollama", "together":
			cfg.OllamaTurbo.Model = model
		case "openrouter":
			cfg.OpenRouter.Model = model
		}
	}
	p, err := build(provider, cfg)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, fmt.Errorf("llm: provider %q indisponível (key ausente?)", provider)
	}
	return p, nil
}

// BuildForWithKey é como BuildFor, mas também injeta a API key e o base_url
// vindos da config GLOBAL (platform_model_config) em vez de só usar o .env.
// Campos vazios mantêm o default do .env (back-compat): apiKey=="" usa a key do
// env do provider; baseURL=="" mantém o default; model=="" mantém o default.
// É o seam que liga "salvar a key na UI" → "o provider passa a funcionar".
func BuildForWithKey(cfg config.LLMConfig, provider, model, apiKey, baseURL string) (Provider, error) {
	set := func(key *string, model2 *string, base *string) {
		if apiKey != "" {
			*key = apiKey
		}
		if model != "" {
			*model2 = model
		}
		if baseURL != "" {
			*base = baseURL
		}
	}
	switch provider {
	case "gemini":
		set(&cfg.Gemini.APIKey, &cfg.Gemini.Model, &cfg.Gemini.BaseURL)
	case "anthropic":
		set(&cfg.Anthropic.APIKey, &cfg.Anthropic.Model, &cfg.Anthropic.BaseURL)
	case "minimax":
		set(&cfg.MiniMax.APIKey, &cfg.MiniMax.Model, &cfg.MiniMax.BaseURL)
	case "ollama-turbo", "ollama", "together":
		set(&cfg.OllamaTurbo.APIKey, &cfg.OllamaTurbo.Model, &cfg.OllamaTurbo.BaseURL)
	case "openrouter":
		set(&cfg.OpenRouter.APIKey, &cfg.OpenRouter.Model, &cfg.OpenRouter.BaseURL)
	default:
		return nil, fmt.Errorf("llm: provider desconhecido: %s", provider)
	}
	p, err := build(provider, cfg)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, fmt.Errorf("llm: provider %q indisponível (key ausente?)", provider)
	}
	return p, nil
}

// BuildForWithFallbacks constrói o provider primário (override provider/model,
// ex. per-org ou roteamento temporal) e o ENVOLVE com a mesma fallback chain do
// Factory (cfg.Fallbacks). Sem isso, um override per-org ou uma query temporal
// ficam presos a UM provider — se ele cair (ex. MiniMax 520), a query falha sem
// tentar o ollama-turbo. Com o router, sobrevivem à queda do primário.
func BuildForWithFallbacks(cfg config.LLMConfig, provider, model string) (Provider, error) {
	primary, err := BuildFor(cfg, provider, model)
	if err != nil {
		return nil, err
	}
	var fallbacks []Provider
	for _, name := range cfg.Fallbacks {
		if name == provider {
			continue // não duplicar o primário
		}
		if p, e := build(name, cfg); e == nil && p != nil {
			fallbacks = append(fallbacks, p)
		}
	}
	return NewRouter(primary, fallbacks...), nil
}

func build(name string, cfg config.LLMConfig) (Provider, error) {
	switch name {
	case "gemini":
		if cfg.Gemini.APIKey == "" {
			return nil, fmt.Errorf("gemini: API_KEY ausente")
		}
		return NewGemini(cfg.Gemini), nil
	case "anthropic":
		if cfg.Anthropic.APIKey == "" {
			return nil, fmt.Errorf("anthropic: API_KEY ausente")
		}
		return NewAnthropic(cfg.Anthropic), nil
	case "minimax":
		if cfg.MiniMax.APIKey == "" {
			return nil, fmt.Errorf("minimax: API_KEY ausente")
		}
		return NewMiniMax(cfg.MiniMax), nil
	// "ollama-turbo" é o nome canônico (Ollama Cloud Turbo, ollama.com).
	// "ollama"/"together" são aliases de back-compat — "together" é LEGADO e
	// enganoso (nunca foi together.xyz). Migrar .env pra LLM_PRIMARY=ollama-turbo.
	case "ollama-turbo", "ollama", "together":
		if cfg.OllamaTurbo.APIKey == "" {
			return nil, fmt.Errorf("ollama-turbo: API_KEY ausente")
		}
		return NewOllamaTurbo(cfg.OllamaTurbo), nil
	case "openrouter":
		if cfg.OpenRouter.APIKey == "" {
			return nil, fmt.Errorf("openrouter: API_KEY ausente")
		}
		return NewOpenRouter(cfg.OpenRouter), nil
	case "":
		return nil, fmt.Errorf("llm: provider sem nome")
	default:
		return nil, fmt.Errorf("llm: provider desconhecido: %s", name)
	}
}
