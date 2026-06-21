package api

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nexusyn/engine/internal/metering"
)

// resolveAgentID delega pro package metering (find-or-create de agent por slug).
// Mantido como wrapper fino pra não trocar os call-sites dos handlers.
func resolveAgentID(ctx context.Context, pool *pgxpool.Pool, orgID int64, slug string) (int64, error) {
	return metering.ResolveAgent(ctx, pool, orgID, slug)
}
