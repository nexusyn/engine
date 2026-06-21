package api

import (
	"context"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nexusyn/engine/internal/metering"
)

// orgSuspended consulta is_org_suspended (SECURITY DEFINER, bypassa RLS). FAIL-OPEN:
// erro no check NÃO bloqueia (resiliência — bug nosso não derruba o cliente). Suspensão
// é independente do NEXUS_ENFORCE_LIMITS (é bloqueio de billing, não cota de observação).
func orgSuspended(ctx context.Context, pool *pgxpool.Pool, orgID int64) bool {
	var s bool
	if err := pool.QueryRow(ctx, "SELECT is_org_suspended($1)", orgID).Scan(&s); err != nil {
		return false
	}
	return s
}

// requireOrgActive escreve 403 e retorna false se a org está suspensa. Gate de billing
// (AUD-010) nos caminhos data-plane.
func requireOrgActive(w http.ResponseWriter, r *http.Request, pool *pgxpool.Pool, orgID int64) bool {
	if orgSuspended(r.Context(), pool, orgID) {
		writeError(w, http.StatusForbidden, "organization suspended — resolve billing to continue")
		return false
	}
	return true
}

// checkQueryQuota aplica a quota mensal de queries (org_usage vs
// org_limits.max_queries) nos caminhos de retrieval (/v1/query, /v1/query/stream,
// /v1/search — search conta na quota por decisão de produto 2026-06-12).
//
// Sempre incrementa o contador (observabilidade); só bloqueia (402) com
// NEXUS_ENFORCE_LIMITS=true. Retorna false se a resposta 402 já foi escrita.
func checkQueryQuota(w http.ResponseWriter, r *http.Request, pool *pgxpool.Pool, orgID int64) bool {
	if !requireOrgActive(w, r, pool, orgID) { // AUD-010: org suspensa → 403 (independe do enforce)
		return false
	}
	quota := metering.CheckAndIncrQuery(r.Context(), pool, orgID, metering.Enforced())
	if !quota.Allowed {
		writeError(w, http.StatusPaymentRequired, fmt.Sprintf(
			"monthly query limit reached (%d/%d) — upgrade your plan for more queries",
			quota.Used, quota.Max))
		return false
	}
	return true
}
