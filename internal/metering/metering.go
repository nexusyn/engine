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
	"time"

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

// Enforced diz se a cota é bloqueante (402) no kill-switch global. Lê o flag em
// memória (reflete o toggle). Memórias seguem só este flag.
func Enforced() bool { return enforced.Load() }

// queryBlockStart é o instante em que queries novas passam a ser bloqueadas
// quando a cota do plano (mais pacotes) acaba, mesmo com NEXUS_ENFORCE_LIMITS
// desligado. 1º de outubro de 2026, meia-noite em São Paulo.
var queryBlockStart = time.Date(2026, 10, 1, 0, 0, 0, 0, saoPaulo())

func saoPaulo() *time.Location {
	loc, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		return time.FixedZone("BRT", -3*60*60)
	}
	return loc
}

// QueryGrace é o aviso antes do corte. O relógio começa quando o e-mail sai
// (o console chama arm_quota_notice). Queries novas só bloqueiam 72h depois
// desse e-mail, se a cota continuar estourada.
const QueryGrace = 72 * time.Hour

// QueryBlockActive diz se a política de corte (com 72h de aviso) está valendo.
// Verdadeiro a partir de queryBlockStart. O kill-switch global, antes dessa
// data, continua bloqueando na hora.
func QueryBlockActive(now time.Time) bool {
	return !now.Before(queryBlockStart)
}

// GraceElapsed diz se o corte já venceu. overSince zero = relógio ainda não armado.
func GraceElapsed(now, overSince time.Time) bool {
	if overSince.IsZero() {
		return false
	}
	return !now.Before(overSince.Add(QueryGrace))
}

// TopupURL é o checkout do pacote de US$ 1 (50 queries + 500 memórias).
func TopupURL() string {
	if u := os.Getenv("NEXUS_TOPUP_URL"); u != "" {
		return u
	}
	return "https://app.nexusyn.ai/billing/topup"
}

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
	Allowed   bool
	Used      int64      // consumo do período após o check (sem incremento quando bloqueado)
	Max       int64      // limite vigente (0 = ilimitado)
	OverSince *time.Time // primeira vez acima da cota; nil = dentro da cota
	BlockAt   *time.Time // instante do e-mail + 72h
}

// CheckAndIncrQuery incrementa o contador mensal de queries da org (org_usage,
// metric='query') e checa contra org_limits.max_queries — atômico, 1 round-trip
// (função SECURITY DEFINER check_and_incr_usage). Com enforce=false só conta;
// com enforce=true e quota estourada retorna Allowed=false SEM incrementar.
//
// Fail-open: erro no check loga warn e LIBERA (bug nosso não derruba cliente).
func CheckAndIncrQuery(ctx context.Context, pool *pgxpool.Pool, orgID int64, enforce bool) QuotaResult {
	res := QuotaResult{Allowed: true}
	now := time.Now()
	var err error
	if QueryBlockActive(now) {
		err = pool.QueryRow(ctx,
			`SELECT allowed, used, max_allowed, over_since, block_at
			   FROM check_and_incr_query_grace($1, $2, true, $3)`,
			orgID, enforce, int(QueryGrace/time.Hour),
		).Scan(&res.Allowed, &res.Used, &res.Max, &res.OverSince, &res.BlockAt)
	} else {
		err = pool.QueryRow(ctx,
			`SELECT allowed, used, max_allowed FROM check_and_incr_usage($1, 'query', $2)`,
			orgID, enforce,
		).Scan(&res.Allowed, &res.Used, &res.Max)
	}
	if err != nil {
		slog.Warn("metering: check de quota falhou (fail-open)", "err", err, "org", orgID)
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

// SanitizeProjectSlug normaliza o slug de PROJETO (memória por projeto, Design A).
// Mesmo formato canônico do agent (lowercase, kebab, ≤64); texto livre por org (sem
// allowlist, ao contrário de domain). Vazio/inválido → "" → pages.project fica NULL
// (= global/geral).
func SanitizeProjectSlug(s string) string {
	return SanitizeAgentSlug(s)
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
