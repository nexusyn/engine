//go:build integration

// ingest_created_at_integration_test.go — gate do IngestArgs.CreatedAt (import):
// quando presente, o INSERT da page preserva a cronologia original (round-trip
// export→import); ausente, mantém o default now() do ingest comum.
package job

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIngest_CreatedAtPreservado(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	ctx := context.Background()
	appURL, adminURL, cleanup := setupPG(ctx, t)
	defer cleanup()
	org := seedOrg(ctx, t, adminURL)

	pool, err := pgxpool.New(ctx, appURL)
	require.NoError(t, err)
	w := NewIngestWorker(pool, nil)

	original := time.Date(2025, 3, 14, 9, 26, 53, 0, time.UTC)

	// Com CreatedAt (caminho do import) → preserva.
	require.NoError(t, w.Work(ctx, &river.Job[IngestArgs]{
		JobRow: &rivertype.JobRow{ID: 1},
		Args: IngestArgs{
			OrganizationID: org, Title: "Importada", Domain: "memory",
			Content: "Memória importada com data original", CreatedAt: &original,
		},
	}))

	adminPool, err := pgxpool.New(ctx, adminURL)
	require.NoError(t, err)
	defer adminPool.Close()

	var got time.Time
	require.NoError(t, adminPool.QueryRow(ctx,
		`SELECT created_at FROM pages WHERE organization_id=$1 AND title='Importada'`, org).Scan(&got))
	assert.True(t, got.Equal(original), "created_at deve ser o original do import, veio %s", got)

	// Sem CreatedAt (ingest comum) → default now().
	require.NoError(t, w.Work(ctx, &river.Job[IngestArgs]{
		JobRow: &rivertype.JobRow{ID: 2},
		Args: IngestArgs{
			OrganizationID: org, Title: "Nova", Domain: "memory",
			Content: "Memória comum sem backdate",
		},
	}))
	var now time.Time
	require.NoError(t, adminPool.QueryRow(ctx,
		`SELECT created_at FROM pages WHERE organization_id=$1 AND title='Nova'`, org).Scan(&now))
	assert.WithinDuration(t, time.Now(), now, time.Minute, "sem CreatedAt mantém now()")
}
