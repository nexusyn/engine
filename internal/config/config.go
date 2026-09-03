// Package config carrega configuração a partir de variáveis de ambiente.
//
// Filosofia: 12-factor — toda config vem do env. Sem yaml/toml. Sem watch.
// Defaults sensatos pra cada campo; valores obrigatórios marcados com `required:"true"`.
//
// Uso:
//
//	cfg, err := config.Load()
//	if err != nil { ... }
//	srv := server.New(cfg)
package config

import (
	"fmt"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/joho/godotenv"
)

// Config é o struct raiz com todas as configurações da aplicação.
// Cada subseção tem prefix próprio pra organizar grupos de envs.
type Config struct {
	HTTP    HTTPConfig
	DB      DBConfig
	LLM     LLMConfig
	Embed   EmbedConfig
	Rerank  RerankConfig
	Vector  VectorConfig
	Compile CompileConfig
	Auth    AuthConfig
	Log     LogConfig
	OTel    OTelConfig
}

type HTTPConfig struct {
	Port         int           `env:"HTTP_PORT"           envDefault:"8044"`
	ReadTimeout  time.Duration `env:"HTTP_READ_TIMEOUT"   envDefault:"30s"`
	WriteTimeout time.Duration `env:"HTTP_WRITE_TIMEOUT"  envDefault:"120s"`
}

type DBConfig struct {
	// URL pra runtime (nexus_app — non-superuser, RLS ativa)
	URL string `env:"DATABASE_URL,required"`
	// URL pra migrations + admin ops (nexus — superuser).
	// Se vazio, usa URL (modo single-user — só funciona se URL aponta pra superuser).
	AdminURL string `env:"ADMIN_DATABASE_URL"`

	// MaxConns default subiu de 20→50 (2026-07-02): pool de 20 era gargalo — cada
	// /v1/query consome várias conns simultâneas (suspensão+quota+rps+tx+auth
	// touch), teto ~10 requests caros concorrentes. É só o fallback; prod ajusta
	// via env no compose.
	MaxConns int32 `env:"DB_MAX_CONNS" envDefault:"50"`
	MinConns int32 `env:"DB_MIN_CONNS" envDefault:"2"`
}

// AdminOrDefault retorna AdminURL se setado, senão fallback pra URL.
// Usado pelo migrate subcommand — em dev é OK usar a mesma URL pra ambos.
func (c DBConfig) AdminOrDefault() string {
	if c.AdminURL != "" {
		return c.AdminURL
	}
	return c.URL
}

type LLMConfig struct {
	Primary   string   `env:"LLM_PRIMARY"   envDefault:"gemini"`
	Fallbacks []string `env:"LLM_FALLBACKS" envDefault:"anthropic,minimax" envSeparator:","`

	Gemini      ProviderConfig `envPrefix:"GEMINI_"`
	Anthropic   ProviderConfig `envPrefix:"ANTHROPIC_"`
	MiniMax     ProviderConfig `envPrefix:"MINIMAX_"`
	OllamaTurbo ProviderConfig `envPrefix:"OLLAMA_"`     // Ollama Cloud Turbo (ollama.com) — NÃO together.xyz
	OpenRouter  ProviderConfig `envPrefix:"OPENROUTER_"` // gateway OpenAI-compat (openrouter.ai) — ex: google/gemini-3.5-flash

	// Extractor dedicado (opcional). O ExtractEntities roda em TODA página
	// (alto volume) — um modelo barato/rápido (ex: google/gemini-3.1-flash-lite
	// via openrouter) acelera o ingest sem o custo do modelo de geração. Vazio
	// = usa o router global (LLM_PRIMARY). Reusa as keys já configuradas.
	ExtractProvider string `env:"EXTRACT_PROVIDER"`
	ExtractModel    string `env:"EXTRACT_MODEL"`
}

type ProviderConfig struct {
	APIKey  string        `env:"API_KEY"`
	Model   string        `env:"MODEL"`
	BaseURL string        `env:"BASE_URL"`
	Timeout time.Duration `env:"TIMEOUT" envDefault:"60s"`

	// RateLimitRPS limita requests por segundo no client-side (token bucket).
	// 0 = sem rate limit. Default por provider abaixo evita 429s em uso normal.
	RateLimitRPS int `env:"RATE_LIMIT_RPS"`
}

type EmbedConfig struct {
	Provider     string `env:"EMBED_PROVIDER"       envDefault:"jina"`
	BatchSize    int    `env:"EMBED_BATCH_SIZE"     envDefault:"100"`
	RateLimitRPS int    `env:"EMBED_RATE_LIMIT_RPS" envDefault:"50"`

	Jina JinaConfig `envPrefix:"JINA_"`
}

// JinaConfig — Jina AI API hospedada (api.jina.ai). Alternativa ao TEI local
// (CPU) sem o gargalo: embed de 32 chunks ~1.6s vs ~33-50s no TEI-CPU; rerank
// ~0.67s vs ~20-68s. jina-embeddings-v5-text-small sai em 1024 dims nativos
// (compat com o schema vector(1024); Matryoshka permite truncar mais).
// GOTCHA: o Cloudflare na frente da Jina barra User-Agent não-browser (erro
// 1010) — os providers jina setam UA de browser.
type JinaConfig struct {
	APIKey     string        `env:"API_KEY"`
	EmbedModel string        `env:"EMBED_MODEL" envDefault:"jina-embeddings-v5-text-small"`
	EmbedDims  int           `env:"EMBED_DIMS"  envDefault:"1024"`
	BaseURL    string        `env:"BASE_URL"    envDefault:"https://api.jina.ai"`
	Timeout    time.Duration `env:"TIMEOUT"     envDefault:"60s"`
}

type RerankConfig struct {
	Driver string `env:"RERANKER" envDefault:"jina"` // jina | none
	// Jina rerank hospedado (RERANKER=jina). Usa a key/baseURL do JinaConfig
	// (embed), passados pela Factory — uma conta Jina serve embed + rerank.
	JinaModel string `env:"JINA_RERANK_MODEL" envDefault:"jina-reranker-v3"`
	// JinaTimeout — timeout HTTP da chamada de rerank ao Jina.
	JinaTimeout time.Duration `env:"JINA_RERANK_TIMEOUT" envDefault:"30s"`
	// JinaMaxTokensPerDoc: teto de tokens/doc no rerank (max_tokens_per_doc, 1-8192,
	// default da API 2048). Chunks ~400 tok (MaxChars=1500) → 2048 nao trunca.
	JinaMaxTokensPerDoc int `env:"JINA_RERANK_MAX_TOKENS_PER_DOC" envDefault:"2048"`
	// JinaTruncate: true → Jina trunca doc longo em vez de errar a request inteira
	// (evita fallback silencioso pro RRF). v3 ja auto-trunca; explicito por robustez.
	JinaTruncate bool `env:"JINA_RERANK_TRUNCATE" envDefault:"true"`
}

type VectorConfig struct {
	IndexType string `env:"VECTOR_INDEX" envDefault:"diskann"` // diskann | hnsw | none
}

type CompileConfig struct {
	AutoThresholdNIngest int      `env:"COMPILE_AUTO_THRESHOLD_NINGEST" envDefault:"20"`
	CronDomains          []string `env:"COMPILE_CRON_DOMAINS"           envDefault:"wiki,knowledge" envSeparator:","`
	// Auto-compile periódico (worker). Interval=0 → DESLIGADO (default, opt-in:
	// não roda em prod até setar COMPILE_INTERVAL). Cada tick processa só as
	// fontes novas desde o watermark, no máx MaxItemsPerRun por org, e até
	// MaxOrgsPerTick orgs por vez (anti-contenção da key MiniMax compartilhada).
	Interval       time.Duration `env:"COMPILE_INTERVAL"           envDefault:"0"`
	MaxItemsPerRun int           `env:"COMPILE_MAX_ITEMS_PER_RUN"  envDefault:"30"`
	MaxOrgsPerTick int           `env:"COMPILE_MAX_ORGS_PER_TICK"  envDefault:"1"`
	Agent          string        `env:"COMPILE_AGENT"              envDefault:"claude"`
}

type AuthConfig struct {
	BCryptCost int `env:"AUTH_BCRYPT_COST" envDefault:"12"`
}

type LogConfig struct {
	Level  string `env:"LOG_LEVEL"  envDefault:"info"` // debug|info|warn|error
	Format string `env:"LOG_FORMAT" envDefault:"json"` // json|text
}

type OTelConfig struct {
	Enabled        bool   `env:"OTEL_ENABLED"               envDefault:"false"`
	Endpoint       string `env:"OTEL_EXPORTER_OTLP_ENDPOINT"`
	ServiceName    string `env:"OTEL_SERVICE_NAME"          envDefault:"nexus"`
	ServiceVersion string `env:"OTEL_SERVICE_VERSION"       envDefault:"dev"`
}

// Load carrega .env (se existir) e parseia a struct Config a partir do ambiente.
// Retorna erro descritivo se algum campo `required:"true"` não estiver setado.
func Load() (*Config, error) {
	// .env é opcional — em prod o env é injetado pelo container.
	// Ignoramos erro de "not found" propositalmente.
	_ = godotenv.Load(".env")

	cfg := &Config{}
	if err := env.Parse(cfg); err != nil {
		return nil, fmt.Errorf("parse env: %w", err)
	}
	return cfg, nil
}
