//go:build integration

// quota_integration_test.go — gate do enforcement de billing (F2).
//
// Prova que check_and_incr_usage (migração 0027):
//  1. Sem org_limits → sempre allowed, contador incrementa (max=0/ilimitado)
//  2. enforce=false → só observa: incrementa MESMO acima do limite
//  3. enforce=true + quota estourada → allowed=false SEM incrementar
//  4. Funciona como nexus_app (grants/SECURITY DEFINER corretos)
//  5. CheckAndIncrQuery é fail-open (pool quebrado → Allowed=true)
//
// Rodar: go test -tags=integration -run TestQuota ./internal/metering/...
package metering_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
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

	"github.com/nexusyn/engine/internal/metering"
	"github.com/nexusyn/engine/migrations"
)

const (
	pgImage   = "pgvector/pgvector:pg18"
	dbName    = "nexus_test"
	adminUser = "nexus_admin"
	adminPwd  = "admin-test"
	appUser   = "nexus_app"
	appPwd    = "app-test"
)

// setupPostgres sobe Postgres efêmero, cria a role nexus_app ANTES das
// migrations (espelha docker/postgres-init.sh) e aplica as migrations.
func setupPostgres(ctx context.Context, t *testing.T) (appURL, adminURL string, cleanup func()) {
	t.Helper()

	pgContainer, err := postgres.Run(ctx, pgImage,
		postgres.WithDatabase(dbName),
		postgres.WithUsername(adminUser),
		postgres.WithPassword(adminPwd),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	require.NoError(t, err, "subir Postgres testcontainer")
	cleanup = func() { _ = pgContainer.Terminate(context.Background()) }

	adminURL, err = pgContainer.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	// Role nexus_app ANTES das migrations (0020+ têm GRANTs à role).
	adminPool, err := pgxpool.New(ctx, adminURL)
	require.NoError(t, err)
	_, err = adminPool.Exec(ctx, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s'`, appUser, appPwd))
	require.NoError(t, err)
	_, err = adminPool.Exec(ctx, `GRANT USAGE ON SCHEMA public TO nexus_app`)
	require.NoError(t, err)

	// Migrations (goose, embed.FS)
	db, err := sql.Open("pgx", adminURL)
	require.NoError(t, err)
	goose.SetBaseFS(migrations.FS)
	require.NoError(t, goose.SetDialect("postgres"))
	goose.SetLogger(goose.NopLogger())
	require.NoError(t, goose.Up(db, "."))
	_ = db.Close()

	// Seed: 1 org de teste.
	_, err = adminPool.Exec(ctx,
		`INSERT INTO organizations (id, slug, name) VALUES (1, 'org-quota', 'Org Quota')`)
	require.NoError(t, err)
	adminPool.Close()

	appURL = strings.Replace(adminURL,
		fmt.Sprintf("%s:%s", adminUser, adminPwd),
		fmt.Sprintf("%s:%s", appUser, appPwd), 1)
	return appURL, adminURL, cleanup
}

func TestQuota_CheckAndIncr(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}
	ctx := context.Background()
	appURL, adminURL, cleanup := setupPostgres(ctx, t)
	defer cleanup()

	// Conecta como nexus_app — valida grants reais (EXECUTE na function).
	appPool, err := pgxpool.New(ctx, appURL)
	require.NoError(t, err)
	defer appPool.Close()

	const org = int64(1)

	// 1) Sem org_limits → ilimitado: allowed, contador anda.
	q := metering.CheckAndIncrQuery(ctx, appPool, org, true)
	assert.True(t, q.Allowed)
	assert.Equal(t, int64(1), q.Used)
	assert.Equal(t, int64(0), q.Max)

	q = metering.CheckAndIncrQuery(ctx, appPool, org, true)
	assert.True(t, q.Allowed)
	assert.Equal(t, int64(2), q.Used)

	// Seta max_queries=3 via set_org_limits (como o console faz).
	adminPool, err := pgxpool.New(ctx, adminURL)
	require.NoError(t, err)
	defer adminPool.Close()
	_, err = adminPool.Exec(ctx, `SELECT set_org_limits($1, 0, 3, 0)`, org)
	require.NoError(t, err)

	// 2) enforce=false → só observa: passa de 3 sem bloquear.
	for i := 0; i < 3; i++ {
		q = metering.CheckAndIncrQuery(ctx, appPool, org, false)
		assert.True(t, q.Allowed, "enforce=false nunca bloqueia")
	}
	assert.Equal(t, int64(5), q.Used, "contador anda mesmo acima do limite")
	assert.Equal(t, int64(3), q.Max)

	// 3) enforce=true + estourado → bloqueia SEM incrementar.
	q = metering.CheckAndIncrQuery(ctx, appPool, org, true)
	assert.False(t, q.Allowed)
	assert.Equal(t, int64(5), q.Used, "request bloqueada não incrementa")

	q = metering.CheckAndIncrQuery(ctx, appPool, org, true)
	assert.False(t, q.Allowed)
	assert.Equal(t, int64(5), q.Used)

	// 4) Limite subiu (upgrade de plano) → volta a passar.
	_, err = adminPool.Exec(ctx, `SELECT set_org_limits($1, 0, 100, 0)`, org)
	require.NoError(t, err)
	q = metering.CheckAndIncrQuery(ctx, appPool, org, true)
	assert.True(t, q.Allowed)
	assert.Equal(t, int64(6), q.Used)

	// Sanidade: org_usage tem exatamente 1 linha (org, mês corrente, 'query').
	var rows int
	require.NoError(t, adminPool.QueryRow(ctx,
		`SELECT count(*) FROM org_usage WHERE organization_id = $1`, org).Scan(&rows))
	assert.Equal(t, 1, rows)
}

func TestQuota_FailOpen(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}
	ctx := context.Background()
	appURL, _, cleanup := setupPostgres(ctx, t)
	defer cleanup()

	pool, err := pgxpool.New(ctx, appURL)
	require.NoError(t, err)
	pool.Close() // pool fechado → erro no check

	q := metering.CheckAndIncrQuery(ctx, pool, 1, true)
	assert.True(t, q.Allowed, "erro no check NUNCA bloqueia cliente (fail-open)")
}
