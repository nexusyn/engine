package api

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nexusyn/engine/internal/metering"
)

// recordUsage delega pro package metering (grava em usage_records). Wrapper fino
// pra manter os call-sites dos handlers simples.
func recordUsage(ctx context.Context, pool *pgxpool.Pool, orgID, agentID int64, metric, provider, model string, latencyMs int, meta map[string]any) {
	metering.Record(ctx, pool, orgID, agentID, metric, provider, model, latencyMs, meta)
}
