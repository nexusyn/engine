package api

import (
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// AdminUsageHandler — GET /v1/admin/usage: visão agregada cross-org de cota vs
// consumo de TODAS as orgs (limites + queries do mês + memórias). Lê via
// admin_usage_overview() (SECURITY DEFINER, bypassa RLS). Backing da página de
// calibração de cota do console. Admin-only (RequireAbility no router).
func AdminUsageHandler(pool *pgxpool.Pool) http.HandlerFunc {
	type orgUsage struct {
		OrganizationID int64  `json:"organization_id"`
		OrgName        string `json:"org_name"`
		MaxPages       int64  `json:"max_pages"`
		MaxQueries     int64  `json:"max_queries"`
		MaxRPS         int    `json:"max_rps"`
		PagesUsed      int64  `json:"pages_used"`
		QueriesUsed    int64  `json:"queries_used"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := pool.Query(r.Context(),
			`SELECT organization_id, org_name, max_pages, max_queries, max_rps, pages_used, queries_used
			 FROM admin_usage_overview()`)
		if err != nil {
			writeInternalError(w, "admin usage", err)
			return
		}
		defer rows.Close()

		out := []orgUsage{}
		for rows.Next() {
			var o orgUsage
			if serr := rows.Scan(&o.OrganizationID, &o.OrgName, &o.MaxPages, &o.MaxQueries,
				&o.MaxRPS, &o.PagesUsed, &o.QueriesUsed); serr != nil {
				writeInternalError(w, "admin usage scan", serr)
				return
			}
			out = append(out, o)
		}
		if rerr := rows.Err(); rerr != nil {
			writeInternalError(w, "admin usage rows", rerr)
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"orgs":   out,
			"period": time.Now().UTC().Format("2006-01"),
		})
	}
}
