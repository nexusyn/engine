package api

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nexusyn/engine/internal/metering"
	"github.com/nexusyn/engine/internal/tenant"
)

// UsageHandler — GET /v1/usage: contagens da org (pages/chunks/entities/edges/
// sessions/tokens). Backing da tela Usage & Billing. Sem metering de cobrança —
// é uso de armazenamento/conteúdo, computado on-demand das tabelas.
func UsageHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, err := tenant.OrgIDFromContext(r.Context())
		if err != nil {
			writeError(w, http.StatusUnauthorized, "no tenant in context")
			return
		}
		var pages, chunks, entities, edges, sessions, tokens int64
		err = tenant.RunWithTenantReadOnly(r.Context(), pool, orgID, func(tx pgx.Tx) error {
			return tx.QueryRow(r.Context(),
				`SELECT
				   (SELECT count(*) FROM pages      WHERE organization_id = $1),
				   (SELECT count(*) FROM chunks     WHERE organization_id = $1),
				   (SELECT count(*) FROM entities   WHERE organization_id = $1),
				   (SELECT count(*) FROM edges      WHERE organization_id = $1),
				   (SELECT count(*) FROM sessions   WHERE organization_id = $1),
				   (SELECT count(*) FROM api_tokens WHERE organization_id = $1)`,
				orgID,
			).Scan(&pages, &chunks, &entities, &edges, &sessions, &tokens)
		})
		if err != nil {
			writeInternalError(w, "usage", err)
			return
		}

		// Breakdown por IA (agent) × métrica, do ledger usage_records — base do
		// relatório/billing por agente ("claude salvou X, cursor fez Y queries").
		byAgent := map[string]map[string]int64{}
		_ = tenant.RunWithTenantReadOnly(r.Context(), pool, orgID, func(tx pgx.Tx) error {
			rows, qerr := tx.Query(r.Context(),
				`SELECT COALESCE(a.slug, 'user') AS agent, ur.metric, count(*)
				 FROM usage_records ur LEFT JOIN agents a ON a.id = ur.agent_id
				 WHERE ur.organization_id = $1
				 GROUP BY 1, ur.metric`, orgID)
			if qerr != nil {
				return qerr
			}
			defer rows.Close()
			for rows.Next() {
				var agent, metric string
				var n int64
				if serr := rows.Scan(&agent, &metric, &n); serr != nil {
					return serr
				}
				if byAgent[agent] == nil {
					byAgent[agent] = map[string]int64{}
				}
				byAgent[agent][metric] = n
			}
			return rows.Err()
		})

		// Quota do período corrente: consumo (org_usage) vs limites (org_limits).
		// 0 = ilimitado. Best-effort: erro deixa o bloco zerado (UI degrada bem).
		var maxPages, maxQueries, queriesUsed int64
		var maxRPS int
		_ = tenant.RunWithTenantReadOnly(r.Context(), pool, orgID, func(tx pgx.Tx) error {
			return tx.QueryRow(r.Context(),
				`SELECT
				   coalesce((SELECT max_pages   FROM org_limits WHERE organization_id = $1), 0),
				   coalesce((SELECT max_queries FROM org_limits WHERE organization_id = $1), 0),
				   coalesce((SELECT max_rps     FROM org_limits WHERE organization_id = $1), 0),
				   coalesce((SELECT used FROM org_usage
				             WHERE organization_id = $1 AND metric = 'query'
				               AND period = date_trunc('month', now())::date), 0)`,
				orgID,
			).Scan(&maxPages, &maxQueries, &maxRPS, &queriesUsed)
		})
		quota := map[string]any{
			"max_pages":    maxPages,
			"pages_used":   pages,
			"max_queries":  maxQueries,
			"queries_used": queriesUsed,
			"max_rps":      maxRPS,
			"period":       time.Now().UTC().Format("2006-01"),
			// false = modo observação (contadores rodam, ninguém é bloqueado) —
			// o console usa isso pra não anunciar bloqueio que não existe.
			"enforced": metering.Enforced(),
		}
		if maxQueries > 0 {
			quota["queries_percent"] = int(queriesUsed * 100 / maxQueries)
		}
		if maxPages > 0 {
			quota["pages_percent"] = int(pages * 100 / maxPages)
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"organization_id": orgID,
			"counts": map[string]int64{
				"pages": pages, "chunks": chunks, "entities": entities,
				"edges": edges, "sessions": sessions, "tokens": tokens,
			},
			"by_agent": byAgent,
			"quota":    quota,
			"as_of":    time.Now().UTC(),
		})
	}
}

type exportPage struct {
	ID        int64     `json:"id"`
	Title     string    `json:"title"`
	Domain    string    `json:"domain"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

// maxExportPages é o teto defensivo do dump (content completo é pesado).
const maxExportPages = 10000

// ExportHandler — GET /v1/export[?ids=1,2,3 | ?domain=X&q=texto]: dump das memórias
// (pages, content completo) da org em JSON. Backing do "export selecionado" do
// dashboard. `ids` (csv) exporta exatamente essas pages (seleção do usuário);
// senão filtra por domain+q (o que está pesquisado, ou tudo). Só pages ativas
// (valid_to IS NULL). Teto defensivo de maxExportPages.
func ExportHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, err := tenant.OrgIDFromContext(r.Context())
		if err != nil {
			writeError(w, http.StatusUnauthorized, "no tenant in context")
			return
		}

		// ids tem precedência (seleção explícita); senão cai no filtro domain+q.
		var where string
		var args []any
		if ids := parseIDList(r.URL.Query().Get("ids")); len(ids) > 0 {
			where = "organization_id = $1 AND valid_to IS NULL AND id = ANY($2)"
			args = []any{orgID, ids}
		} else {
			where, args = pageFilter(orgID, r.URL.Query().Get("domain"),
				strings.TrimSpace(r.URL.Query().Get("q")), "")
		}

		out := []exportPage{}
		err = tenant.RunWithTenantReadOnly(r.Context(), pool, orgID, func(tx pgx.Tx) error {
			rows, qerr := tx.Query(r.Context(), fmt.Sprintf(
				`SELECT id, title, domain, content, created_at
				 FROM pages WHERE %s ORDER BY created_at DESC LIMIT %d`, where, maxExportPages), args...)
			if qerr != nil {
				return qerr
			}
			defer rows.Close()
			for rows.Next() {
				var p exportPage
				if serr := rows.Scan(&p.ID, &p.Title, &p.Domain, &p.Content, &p.CreatedAt); serr != nil {
					return serr
				}
				out = append(out, p)
			}
			return rows.Err()
		})
		if err != nil {
			writeInternalError(w, "export", err)
			return
		}
		w.Header().Set("Content-Disposition", "attachment; filename=nexus-export.json")
		writeJSON(w, http.StatusOK, map[string]any{
			"organization_id": orgID,
			"count":           len(out),
			"exported_at":     time.Now().UTC(),
			"pages":           out,
		})
	}
}
