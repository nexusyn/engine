//go:build integration

// admin_delete_integration_test.go — gate do purge de org (exclusão de conta LGPD).
//
// Prova que delete_org (migração 0028) apaga a org E todos os dados dependentes
// via cascade (pages, agents, org_limits, org_usage, usage_records) numa só
// operação, e que é idempotente (org inexistente → 0).
//
// Rodar: go test -tags=integration -run TestDeleteOrg ./internal/api/...
package api_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/nexusyn/engine/migrations"
)

const (
	pgImage   = "pgvector/pgvector:pg17"
	dbName    = "nexus_test"
	adminUser = "nexus_admin"
	adminPwd  = "admin-test"
	appUser   = "nexus_app"
	appPwd    = "app-test"
)

func setupPostgres(ctx context.Context, t *testing.T) (*pgxpool.Pool, func()) {
	t.Helper()

	pgContainer, err := postgres.Run(ctx, pgImage,
		postgres.WithDatabase(dbName),
		postgres.WithUsername(adminUser),
		postgres.WithPassword(adminPwd),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second),
		),
	)
	require.NoError(t, err, "subir Postgres testcontainer")
	cleanup := func() { _ = pgContainer.Terminate(context.Background()) }

	adminURL, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	// Role nexus_app ANTES das migrations (0020+ fazem GRANT à role).
	adminPool, err := pgxpool.New(ctx, adminURL)
	require.NoError(t, err)
	_, err = adminPool.Exec(ctx, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s'`, appUser, appPwd))
	require.NoError(t, err)
	_, err = adminPool.Exec(ctx, `GRANT USAGE ON SCHEMA public TO nexus_app`)
	require.NoError(t, err)

	db, err := sql.Open("pgx", adminURL)
	require.NoError(t, err)
	goose.SetBaseFS(migrations.FS)
	require.NoError(t, goose.SetDialect("postgres"))
	goose.SetLogger(goose.NopLogger())
	require.NoError(t, goose.Up(db, "."))
	_ = db.Close()

	return adminPool, func() { adminPool.Close(); cleanup() }
}

func TestDeleteOrg_PurgesEverything(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}
	ctx := context.Background()
	pool, cleanup := setupPostgres(ctx, t)
	defer cleanup()

	const org = int64(1)

	// Seed: org + agent + page + chunk + limits + usage (amostra das tabelas).
	_, err := pool.Exec(ctx, `INSERT INTO organizations (id, slug, name) VALUES ($1, 'acme', 'Acme')`, org)
	require.NoError(t, err)
	var agentID int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO agents (organization_id, slug, name) VALUES ($1, 'claude', 'claude') RETURNING id`, org).Scan(&agentID))
	var pageID int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO pages (organization_id, agent_id, slug, title, content, domain)
		 VALUES ($1, $2, 'p1', 'T', 'C', 'memory') RETURNING id`, org, agentID).Scan(&pageID))
	_, err = pool.Exec(ctx,
		`INSERT INTO chunks (organization_id, page_id, position, content) VALUES ($1, $2, 0, 'C')`, org, pageID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `SELECT set_org_limits($1, 100, 50, 5)`, org)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `SELECT check_and_incr_usage($1, 'query', false)`, org)
	require.NoError(t, err)

	// Sanidade: tudo presente.
	assert.Equal(t, 1, count(t, ctx, pool, "pages", org))
	assert.Equal(t, 1, count(t, ctx, pool, "chunks", org))
	assert.Equal(t, 1, count(t, ctx, pool, "agents", org))
	assert.Equal(t, 1, count(t, ctx, pool, "org_limits", org))
	assert.Equal(t, 1, count(t, ctx, pool, "org_usage", org))

	// Purge.
	var deleted int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT delete_org($1)`, org).Scan(&deleted))
	assert.Equal(t, int64(1), deleted)

	// Tudo sumiu (cascade), inclusive a própria org.
	for _, tbl := range []string{"pages", "chunks", "agents", "org_limits", "org_usage"} {
		assert.Equalf(t, 0, count(t, ctx, pool, tbl, org), "tabela %s deveria estar vazia pós-purge", tbl)
	}
	var orgs int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM organizations WHERE id = $1`, org).Scan(&orgs))
	assert.Equal(t, 0, orgs)

	// Idempotente: rodar de novo → 0 (não existe mais), sem erro.
	require.NoError(t, pool.QueryRow(ctx, `SELECT delete_org($1)`, org).Scan(&deleted))
	assert.Equal(t, int64(0), deleted)
}

func count(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string, org int64) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(ctx,
		fmt.Sprintf(`SELECT count(*) FROM %s WHERE organization_id = $1`, table), org).Scan(&n))
	return n
}
