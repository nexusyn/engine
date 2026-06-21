// Package metering centraliza (a) a resolução de agent por slug (find-or-create)
// e (b) o registro de uso em usage_records — a base do billing e do relatório
// de uso por IA. Compartilhado entre o HTTP (api) e o MCP (mcp), pra que ambos
// atribuam memória/uso à mesma IA (org + agent).
package metering

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nexusyn/engine/internal/tenant"
)

// Enforced reporta o kill-switch global do enforcement de quota
// (NEXUS_ENFORCE_LIMITS). OFF por padrão → contadores rodam, ninguém é
// bloqueado. Fonte única da flag — usada pelo ingest (storage), pelos
// caminhos de query (HTTP + MCP) e pelo rate limit.
// enforced — flag GLOBAL de enforcement de cota, TOGGLÁVEL em runtime pelo admin
// (botão no console → PUT /v1/admin/settings/enforcement → SetEnforced + grava no DB).
// Semeado do .env no boot (init) e do platform_settings (RefreshEnforced, no boot do
// engine — persiste o toggle entre restarts). BETA: default OFF (ninguém com cota limite).
var enforced atomic.Bool

func init() { enforced.Store(os.Getenv("NEXUS_ENFORCE_LIMITS") == "true") }

// Enforced diz se a cota é bloqueante (402). Lê o flag em memória (reflete o toggle).
func Enforced() bool { return enforced.Load() }

// SetEnforced atualiza o flag em memória (chamado pelo handler admin após gravar no DB).
func SetEnforced(v bool) { enforced.Store(v) }

// RefreshEnforced carrega o flag do platform_settings (chamado no boot). Sem linha →
// mantém o valor do .env; erro → mantém o atual (fail-safe).
func RefreshEnforced(ctx context.Context, pool *pgxpool.Pool) {
	if pool == nil {
		return
	}
	var s string
	if err := pool.QueryRow(ctx, "SELECT value FROM platform_settings WHERE key = 'enforce_limits'").Scan(&s); err == nil {
		enforced.Store(s == "true")
	}
}

// QuotaResult é o veredito do check de quota mensal.
type QuotaResult struct {
	Allowed bool
	Used    int64 // consumo do período após o check (sem incremento quando bloqueado)
	Max     int64 // limite vigente (0 = ilimitado)
}

// CheckAndIncrQuery incrementa o contador mensal de queries da org (org_usage,
// metric='query') e checa contra org_limits.max_queries — atômico, 1 round-trip
// (função SECURITY DEFINER check_and_incr_usage). Com enforce=false só conta;
// com enforce=true e quota estourada retorna Allowed=false SEM incrementar.
//
// Fail-open: erro no check loga warn e LIBERA (bug nosso não derruba cliente).
func CheckAndIncrQuery(ctx context.Context, pool *pgxpool.Pool, orgID int64, enforce bool) QuotaResult {
	res := QuotaResult{Allowed: true}
	err := pool.QueryRow(ctx,
		`SELECT allowed, used, max_allowed FROM check_and_incr_usage($1, 'query', $2)`,
		orgID, enforce,
	).Scan(&res.Allowed, &res.Used, &res.Max)
	if err != nil {
		slog.Warn("metering: check_and_incr_usage falhou (fail-open)", "err", err, "org", orgID)
		return QuotaResult{Allowed: true}
	}
	return res
}

// ResolveAgent mapeia um slug de agent (ex: "claude", "gemini", "cursor",
// "minimax") pro id numérico, criando o agent na org se ainda não existir
// (find-or-create idempotente). Slug vazio → 0 (sem agent atribuído).
//
// É a peça que AMARRA a memória à IA fonte: {"agent":"claude"} no ingest →
// NEXUS resolve/cria o agent na org. unique (organization_id, slug).
// agentSlugInvalid casa runs de chars fora de [a-z0-9_-].
var agentSlugInvalid = regexp.MustCompile(`[^a-z0-9_-]+`)

// SanitizeAgentSlug normaliza o slug do agente (anti-spoofing de atribuição/proveniência):
// lowercase, troca char inválido por '-', colapsa, apara hífens e trunca em 64. Vazio → "".
// Agentes legítimos (claude/codex/cursor/hermes) já são válidos → inalterados.
func SanitizeAgentSlug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = agentSlugInvalid.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 64 {
		s = strings.Trim(s[:64], "-")
	}
	return s
}

func ResolveAgent(ctx context.Context, pool *pgxpool.Pool, orgID int64, slug string) (int64, error) {
	slug = SanitizeAgentSlug(slug)
	if slug == "" {
		return 0, nil
	}
	var id int64
	err := tenant.RunWithTenant(ctx, pool, orgID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO agents (organization_id, slug, name)
			 VALUES ($1, $2, $2)
			 ON CONFLICT (organization_id, slug) DO UPDATE SET updated_at = now()
			 RETURNING id`,
			orgID, slug,
		).Scan(&id)
	})
	return id, err
}

// Record grava uma linha no ledger usage_records (org + agent + metric +
// provider/model + latência + metadata). Cada ingest/query vira um evento.
//
// Best-effort: loga erro mas NUNCA falha a request (billing não derruba o
// produto). value=1 por evento; tokens vão no metadata.
func Record(ctx context.Context, pool *pgxpool.Pool, orgID, agentID int64, metric, provider, model string, latencyMs int, meta map[string]any) {
	metaJSON := []byte("{}")
	if len(meta) > 0 {
		if b, err := json.Marshal(meta); err == nil {
			metaJSON = b
		}
	}
	// Colunas nullable: nil quando vazio (em vez de 0/"").
	var agentArg, provArg, modelArg, latArg any
	if agentID > 0 {
		agentArg = agentID
	}
	if provider != "" {
		provArg = provider
	}
	if model != "" {
		modelArg = model
	}
	if latencyMs > 0 {
		latArg = latencyMs
	}

	err := tenant.RunWithTenant(ctx, pool, orgID, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx,
			`INSERT INTO usage_records (organization_id, agent_id, metric, provider, model, value, latency_ms, metadata)
			 VALUES ($1, $2, $3, $4, $5, 1, $6, $7)`,
			orgID, agentArg, metric, provArg, modelArg, latArg, metaJSON,
		)
		return e
	})
	if err != nil {
		slog.Warn("metering: usage_records insert falhou", "err", err, "metric", metric)
	}
}
