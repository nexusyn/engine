//go:build integration

// ingest_dedup_integration_test.go — Módulo A: gate do dedup determinístico.
//
// Sobe Postgres efêmero, aplica as migrations (inclui 0037 content_hash) e valida
// que o IngestWorker NÃO duplica memória idêntica (mesmo org+domain+content_hash),
// trata variação trivial (case/whitespace) como a mesma, e cria page nova pra
// conteúdo distinto ou domain distinto.
//
// setupPG é duplicado por arquivo (padrão do projeto — ver persist_integration_test.go).
package job

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
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

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

func setupPG(ctx context.Context, t *testing.T) (string, string, func()) {
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
	require.NoError(t, err)
	cleanup := func() { _ = pgContainer.Terminate(context.Background()) }
	adminURL, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	rolePool, err := pgxpool.New(ctx, adminURL)
	require.NoError(t, err)
	_, err = rolePool.Exec(ctx, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s'`, appUser, appPwd))
	require.NoError(t, err)
	rolePool.Close()
	db, err := sql.Open("pgx", adminURL)
	require.NoError(t, err)
	goose.SetBaseFS(migrations.FS)
	require.NoError(t, goose.SetDialect("postgres"))
	goose.SetLogger(goose.NopLogger())
	require.NoError(t, goose.Up(db, "."))
	_ = db.Close()
	pool, err := pgxpool.New(ctx, adminURL)
	require.NoError(t, err)
	for _, s := range []string{
		`GRANT USAGE ON SCHEMA public TO nexus_app`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO nexus_app`,
		`GRANT USAGE ON ALL SEQUENCES IN SCHEMA public TO nexus_app`,
		`GRANT EXECUTE ON FUNCTION find_api_token_by_hash(TEXT) TO nexus_app`,
		`ALTER ROLE nexus_app SET search_path = public, pg_temp`,
	} {
		_, err := pool.Exec(ctx, s)
		require.NoError(t, err)
	}
	pool.Close()
	appURL := strings.Replace(adminURL,
		fmt.Sprintf("%s:%s", adminUser, adminPwd),
		fmt.Sprintf("%s:%s", appUser, appPwd),
		1,
	)
	return appURL, adminURL, cleanup
}

func seedOrg(ctx context.Context, t *testing.T, adminURL string) int64 {
	t.Helper()
	pool, err := pgxpool.New(ctx, adminURL)
	require.NoError(t, err)
	defer pool.Close()
	var orgID int64 = 1
	_, err = pool.Exec(ctx, `INSERT INTO organizations (id, slug, name) VALUES ($1, 'org-a', 'Org A')`, orgID)
	require.NoError(t, err)
	return orgID
}

func countCurrentPages(ctx context.Context, t *testing.T, adminURL string, org int64) int {
	t.Helper()
	pool, err := pgxpool.New(ctx, adminURL)
	require.NoError(t, err)
	defer pool.Close()
	var n int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM pages WHERE organization_id=$1 AND valid_to IS NULL`, org).Scan(&n))
	return n
}

func ingestJob(id, org int64, title, content, domain string) *river.Job[IngestArgs] {
	return &river.Job[IngestArgs]{
		JobRow: &rivertype.JobRow{ID: id},
		Args:   IngestArgs{OrganizationID: org, Title: title, Content: content, Domain: domain},
	}
}

func TestIngest_DedupContentHash(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	ctx := context.Background()
	appURL, adminURL, cleanup := setupPG(ctx, t)
	defer cleanup()
	org := seedOrg(ctx, t, adminURL)

	// pool NÃO é fechado de propósito: o Work dispara goroutine detached de enqueue
	// que usa o pool; fechar aqui poderia correr com ela. O cleanup do container mata tudo.
	pool, err := pgxpool.New(ctx, appURL)
	require.NoError(t, err)
	w := NewIngestWorker(pool, nil) // resolver nil → testa só o dedup determinístico

	const txt = "Luciano adora pizza de calabresa"

	require.NoError(t, w.Work(ctx, ingestJob(1, org, "Pizza", txt, "memory")))
	assert.Equal(t, 1, countCurrentPages(ctx, t, adminURL, org), "1ª ingestão cria a page")

	// idêntica → NOOP (mesmo content_hash vigente)
	require.NoError(t, w.Work(ctx, ingestJob(2, org, "Pizza outra vez", txt, "memory")))
	assert.Equal(t, 1, countCurrentPages(ctx, t, adminURL, org), "memória idêntica não duplica")

	// variação só de case/whitespace → NOOP (normalização no hash)
	require.NoError(t, w.Work(ctx, ingestJob(3, org, "x", "  Luciano   ADORA Pizza de Calabresa ", "memory")))
	assert.Equal(t, 1, countCurrentPages(ctx, t, adminURL, org), "variação trivial não duplica")

	// conteúdo distinto → ADD
	require.NoError(t, w.Work(ctx, ingestJob(4, org, "Sushi", "Luciano também gosta de sushi", "memory")))
	assert.Equal(t, 2, countCurrentPages(ctx, t, adminURL, org), "conteúdo novo cria page")

	// mesmo conteúdo, domain diferente → ADD (dedup é por (org, domain, content_hash)).
	// "knowledge" é domain de escrita válido (wiki/lesson/etc. seriam normalizados pra "memory").
	require.NoError(t, w.Work(ctx, ingestJob(5, org, "Pizza knowledge", txt, "knowledge")))
	assert.Equal(t, 3, countCurrentPages(ctx, t, adminURL, org), "domain diferente (knowledge) não é duplicata")
}
