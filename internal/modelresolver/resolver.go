// Package modelresolver resolve, ao vivo, qual provider usar em cada etapa do
// pipeline (generation/extraction/embed/rerank) a partir da config GLOBAL do
// operador (platform_model_config) com fallback pro default do .env.
//
// É a fonte ÚNICA de resolução — garante que, por exemplo, o embed da QUERY e o
// embed do WORKER usem exatamente o mesmo modelo (senão o retrieval quebra).
// Cacheia por assinatura (provider|model|base_url|key); rebuilda só quando muda,
// então edições na UI passam a valer sem restart.
package modelresolver

import (
	"context"
	"hash/fnv"
	"log/slog"
	"strconv"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nexusyn/engine/internal/config"
	"github.com/nexusyn/engine/internal/platformconfig"
	"github.com/nexusyn/engine/internal/provider/embed"
	"github.com/nexusyn/engine/internal/provider/llm"
	"github.com/nexusyn/engine/internal/provider/rerank"
	"github.com/nexusyn/engine/internal/secret"
)

// Resolver resolve providers por etapa. Seguro pra uso concorrente.
type Resolver struct {
	pool   *pgxpool.Pool
	cipher *secret.Cipher
	cfg    config.Config

	// defaults do .env (já construídos no startup). Usados quando não há linha
	// em platform_model_config pra etapa, ou em qualquer falha (graceful).
	defGen     llm.Provider
	defExtract llm.Provider
	defEmbed   embed.Provider
	defRerank  rerank.Provider

	mu    sync.Mutex
	cache map[string]cached // stage -> {sig, provider}
}

type cached struct {
	sig string
	val any
}

// New constrói o resolver. cipher nil OU pool nil → sempre usa os defaults.
func New(pool *pgxpool.Pool, cipher *secret.Cipher, cfg config.Config, defGen, defExtract llm.Provider, defEmbed embed.Provider, defRerank rerank.Provider) *Resolver {
	return &Resolver{
		pool: pool, cipher: cipher, cfg: cfg,
		defGen: defGen, defExtract: defExtract, defEmbed: defEmbed, defRerank: defRerank,
		cache: map[string]cached{},
	}
}

// loaded busca a config global da etapa; retorna nil se desabilitado/ausente.
func (r *Resolver) loaded(ctx context.Context, stage string) *platformconfig.Config {
	if r == nil || r.cipher == nil || r.pool == nil {
		return nil
	}
	c, err := platformconfig.Get(ctx, r.pool, r.cipher, stage)
	if err != nil || c == nil || c.Provider == "" {
		return nil
	}
	return c
}

func sig(c *platformconfig.Config) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(c.Provider + "\x00" + c.Model + "\x00" + c.BaseURL + "\x00" + c.APIKey))
	return strconv.FormatUint(h.Sum64(), 16)
}

// get devolve do cache se a assinatura bate; senão chama build e cacheia.
func (r *Resolver) get(stage, s string, build func() (any, error)) any {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.cache[stage]; ok && c.sig == s && c.val != nil {
		return c.val
	}
	v, err := build()
	if err != nil || v == nil {
		slog.Debug("modelresolver: build falhou, usando default .env", "stage", stage, "err", err)
		return nil
	}
	r.cache[stage] = cached{sig: s, val: v}
	return v
}

// Generation resolve o provider de geração (platform > .env).
func (r *Resolver) Generation(ctx context.Context) llm.Provider {
	c := r.loaded(ctx, "generation")
	if c == nil {
		return r.defGen
	}
	if v := r.get("generation", sig(c), func() (any, error) {
		return llm.BuildForWithKey(r.cfg.LLM, c.Provider, c.Model, c.APIKey, c.BaseURL)
	}); v != nil {
		return v.(llm.Provider)
	}
	return r.defGen
}

// Extraction resolve o extractor (platform > .env). Seguro trocar ao vivo —
// afeta só ingests futuros.
func (r *Resolver) Extraction(ctx context.Context) llm.Provider {
	c := r.loaded(ctx, "extraction")
	if c == nil {
		return r.defExtract
	}
	if v := r.get("extraction", sig(c), func() (any, error) {
		return llm.BuildForWithKey(r.cfg.LLM, c.Provider, c.Model, c.APIKey, c.BaseURL)
	}); v != nil {
		return v.(llm.Provider)
	}
	return r.defExtract
}

// Embed resolve o provider de embed (platform > .env). CRÍTICO: query e worker
// chamam o MESMO resolver → mesmo modelo. Trocar exige re-index (ver reindex).
func (r *Resolver) Embed(ctx context.Context) embed.Provider {
	c := r.loaded(ctx, "embed")
	if c == nil {
		return r.defEmbed
	}
	if v := r.get("embed", sig(c), func() (any, error) {
		return embed.BuildForKey(r.cfg.Embed, c.Provider, c.Model, c.APIKey, c.BaseURL)
	}); v != nil {
		return v.(embed.Provider)
	}
	return r.defEmbed
}

// Reranker resolve o reranker (platform > .env). Seguro trocar ao vivo (re-score
// stateless). Pode retornar nil (rerank desabilitado).
func (r *Resolver) Reranker(ctx context.Context) rerank.Provider {
	c := r.loaded(ctx, "rerank")
	if c == nil {
		return r.defRerank
	}
	if v := r.get("rerank", sig(c), func() (any, error) {
		return rerank.BuildForKey(r.cfg.Rerank, r.cfg.Embed, c.Provider, c.Model, c.APIKey, c.BaseURL)
	}); v != nil {
		return v.(rerank.Provider)
	}
	return r.defRerank
}
