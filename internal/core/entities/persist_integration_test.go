//go:build integration

// persist_integration_test.go — Day 20 gate do supersede bi-temporal.
//
// Sobe Postgres efêmero, aplica migrations, e valida que:
//   - Edge 1-to-1 (located_in) é superseded quando nova chega com to_id distinto
//   - Edge M-to-M (mentions) NÃO causa supersede; ambas coexistem
package entities_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/nexusyn/engine/internal/core/entities"
	"github.com/nexusyn/engine/internal/tenant"
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
	// Role nexus_app ANTES das migrations — espelha docker/postgres-init.sh
	// (0020+ fazem GRANT incondicional a nexus_app e falham sem a role).
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

// seedOrgAndPage cria a org + 1 page placeholder pra usar como source_page_id.
func seedOrgAndPage(ctx context.Context, t *testing.T, adminURL string) (orgID, pageID int64) {
	t.Helper()
	pool, err := pgxpool.New(ctx, adminURL)
	require.NoError(t, err)
	defer pool.Close()
	orgID = 1
	_, err = pool.Exec(ctx, `INSERT INTO organizations (id, slug, name) VALUES ($1, 'org-a', 'Org A')`, orgID)
	require.NoError(t, err)
	err = pool.QueryRow(ctx, `
		INSERT INTO pages (organization_id, slug, title, content, domain)
		VALUES ($1, 'p1', 'P1', 'placeholder', 'memory') RETURNING id
	`, orgID).Scan(&pageID)
	require.NoError(t, err)
	return
}

func TestPersist_SupersedeOneToOne(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	ctx := context.Background()
	appURL, adminURL, cleanup := setupPG(ctx, t)
	defer cleanup()

	orgID, pageID := seedOrgAndPage(ctx, t, adminURL)
	pool, err := pgxpool.New(ctx, appURL)
	require.NoError(t, err)
	defer pool.Close()

	// Persist 1: Luna mora em Lavras-MG
	extracted1 := &entities.Extracted{
		Entities: []entities.EntityRef{
			{Name: "Luna", Kind: "person"},
			{Name: "Lavras-MG", Kind: "place"},
		},
		Edges: []entities.EdgeRef{
			{FromName: "Luna", ToName: "Lavras-MG", Kind: "located_in", Weight: 1.0},
		},
	}
	stats1, err := entities.RunInTenantTx(ctx, pool, orgID, pageID, extracted1)
	require.NoError(t, err)
	assert.Equal(t, 2, stats1.EntitiesInserted)
	assert.Equal(t, 1, stats1.EdgesInserted)
	assert.Equal(t, 0, stats1.EdgesSuperseded, "primeira persist não invalida nada")

	// Persist 2: Luna se mudou pra Florianópolis
	extracted2 := &entities.Extracted{
		Entities: []entities.EntityRef{
			{Name: "Luna", Kind: "person"},         // já existe — UPSERT
			{Name: "Florianópolis", Kind: "place"}, // nova
		},
		Edges: []entities.EdgeRef{
			{FromName: "Luna", ToName: "Florianópolis", Kind: "located_in", Weight: 1.0},
		},
	}
	stats2, err := entities.RunInTenantTx(ctx, pool, orgID, pageID, extracted2)
	require.NoError(t, err)
	assert.Equal(t, 1, stats2.EntitiesInserted, "florianopolis inserida")
	assert.Equal(t, 1, stats2.EntitiesExisting, "luna já existia")
	assert.Equal(t, 1, stats2.EdgesInserted, "nova edge → florianopolis")
	assert.Equal(t, 1, stats2.EdgesSuperseded, "edge antiga (luna→lavras) invalidada")

	// Verificar DB: 2 edges no total, 1 ativa (florianopolis), 1 invalidada (lavras)
	adminPool, err := pgxpool.New(ctx, adminURL)
	require.NoError(t, err)
	defer adminPool.Close()
	var totalEdges, activeEdges int
	require.NoError(t, adminPool.QueryRow(ctx,
		`SELECT COUNT(*), COUNT(*) FILTER (WHERE valid_to IS NULL) FROM edges`).
		Scan(&totalEdges, &activeEdges))
	assert.Equal(t, 2, totalEdges, "2 edges total (1 invalidada, 1 ativa)")
	assert.Equal(t, 1, activeEdges, "só 1 edge ativa (located_in atual)")

	// A ativa deve apontar pra florianópolis
	var activeToSlug string
	err = tenant.RunWithTenantReadOnly(ctx, pool, orgID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT e.slug FROM edges ed
			JOIN entities e ON e.id = ed.to_entity_id
			WHERE ed.valid_to IS NULL AND ed.kind = 'located_in'
		`).Scan(&activeToSlug)
	})
	require.NoError(t, err)
	assert.Equal(t, "florianopolis", activeToSlug)
}

func TestPersist_NoSupersede_ForManyToMany(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	ctx := context.Background()
	appURL, adminURL, cleanup := setupPG(ctx, t)
	defer cleanup()
	orgID, pageID := seedOrgAndPage(ctx, t, adminURL)
	pool, err := pgxpool.New(ctx, appURL)
	require.NoError(t, err)
	defer pool.Close()

	// 2 persists com mesmo from+kind ("mentions" é M-to-M) e to diferentes
	// → ambas ativas, NENHUMA superseded
	for i, target := range []string{"Marte", "Vênus"} {
		extracted := &entities.Extracted{
			Entities: []entities.EntityRef{
				{Name: "Luna", Kind: "person"},
				{Name: target, Kind: "place"},
			},
			Edges: []entities.EdgeRef{
				{FromName: "Luna", ToName: target, Kind: "mentions", Weight: 1.0},
			},
		}
		stats, err := entities.RunInTenantTx(ctx, pool, orgID, pageID, extracted)
		require.NoError(t, err)
		assert.Equal(t, 0, stats.EdgesSuperseded, "iter %d: mentions é M-to-M, sem supersede", i)
		assert.Equal(t, 1, stats.EdgesInserted)
	}

	adminPool, err := pgxpool.New(ctx, adminURL)
	require.NoError(t, err)
	defer adminPool.Close()
	var activeEdges int
	require.NoError(t, adminPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM edges WHERE valid_to IS NULL`).Scan(&activeEdges))
	assert.Equal(t, 2, activeEdges, "ambas edges mentions coexistem ativas")
}

func TestPersist_BatchContradictoryNoZombie(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	// Day 21: LLM extrai 2 edges located_in contraditórias na MESMA passada.
	// Antes do dedup batch-aware: gerava 1 edge zombie (duration 0).
	// Agora: só a última do batch é inserida.
	ctx := context.Background()
	appURL, adminURL, cleanup := setupPG(ctx, t)
	defer cleanup()
	orgID, pageID := seedOrgAndPage(ctx, t, adminURL)
	pool, err := pgxpool.New(ctx, appURL)
	require.NoError(t, err)
	defer pool.Close()

	// Simula: "Luna morava em Lavras-MG, agora vive em Florianópolis"
	// Gemini extrairia AMBAS as located_in no mesmo extract.
	extracted := &entities.Extracted{
		Entities: []entities.EntityRef{
			{Name: "Luna", Kind: "person"},
			{Name: "Lavras-MG", Kind: "place"},
			{Name: "Florianópolis", Kind: "place"},
		},
		Edges: []entities.EdgeRef{
			{FromName: "Luna", ToName: "Lavras-MG", Kind: "located_in", Weight: 1.0},
			{FromName: "Luna", ToName: "Florianópolis", Kind: "located_in", Weight: 1.0},
		},
	}
	stats, err := entities.RunInTenantTx(ctx, pool, orgID, pageID, extracted)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.EdgesInserted, "só a última located_in inserida")
	assert.Equal(t, 0, stats.EdgesSuperseded, "primeira foi filtrada antes do INSERT")

	// Verifica DB: 1 edge total (florianopolis), 0 zombies (valid_from = valid_to)
	adminPool, err := pgxpool.New(ctx, adminURL)
	require.NoError(t, err)
	defer adminPool.Close()
	var totalEdges, zombieEdges int
	require.NoError(t, adminPool.QueryRow(ctx,
		`SELECT COUNT(*), COUNT(*) FILTER (WHERE valid_to IS NOT NULL AND valid_to = valid_from) FROM edges`).
		Scan(&totalEdges, &zombieEdges))
	assert.Equal(t, 1, totalEdges, "exatamente 1 edge no DB")
	assert.Equal(t, 0, zombieEdges, "zero edges zombies")
}

func TestPersist_NoSupersede_WhenSameTarget(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	// Re-persistir mesma edge (mesmo from+kind+to) NÃO deve invalidar a anterior
	// — só edges com to_id DIFERENTE são superseded.
	ctx := context.Background()
	appURL, adminURL, cleanup := setupPG(ctx, t)
	defer cleanup()
	orgID, pageID := seedOrgAndPage(ctx, t, adminURL)
	pool, err := pgxpool.New(ctx, appURL)
	require.NoError(t, err)
	defer pool.Close()

	extracted := &entities.Extracted{
		Entities: []entities.EntityRef{
			{Name: "Luna", Kind: "person"},
			{Name: "Lavras-MG", Kind: "place"},
		},
		Edges: []entities.EdgeRef{
			{FromName: "Luna", ToName: "Lavras-MG", Kind: "located_in", Weight: 1.0},
		},
	}
	_, err = entities.RunInTenantTx(ctx, pool, orgID, pageID, extracted)
	require.NoError(t, err)

	// Segunda persist da MESMA edge (reingest da mesma page, por exemplo)
	stats, err := entities.RunInTenantTx(ctx, pool, orgID, pageID, extracted)
	require.NoError(t, err)
	assert.Equal(t, 0, stats.EdgesSuperseded, "mesmo to_id não dispara supersede")
	assert.Equal(t, 1, stats.EdgesInserted, "segunda edge é inserida (bi-temporal aceita duplicatas)")

	adminPool, err := pgxpool.New(ctx, adminURL)
	require.NoError(t, err)
	defer adminPool.Close()
	var activeEdges int
	require.NoError(t, adminPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM edges WHERE valid_to IS NULL`).Scan(&activeEdges))
	assert.Equal(t, 2, activeEdges, "ambas mantidas (re-afirmação OK)")
}
