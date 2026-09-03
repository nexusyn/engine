//go:build integration

// graph_integration_test.go — Day 16/17 (multi-hop + temporal).
//
// Seed manual de entities + edges, varia opts e checa hops retornados.
// Não usa LLM nem River — só DB + graph.Traverse.
package graph_test

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

	"github.com/nexusyn/engine/internal/core/graph"
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

// setupPG sobe Postgres efêmero + migrations + role nexus_app.
// Replica do pattern em ingest/rls_integration_test sem dependência cruzada.
func setupPG(ctx context.Context, t *testing.T) (appURL, adminURL string, cleanup func()) {
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
	cleanup = func() { _ = pgContainer.Terminate(context.Background()) }

	adminURL, err = pgContainer.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	// Role nexus_app ANTES das migrations — espelha docker/postgres-init.sh
	// (0020+ fazem GRANT incondicional a nexus_app e falham sem a role).
	rolePool, err := pgxpool.New(ctx, adminURL)
	require.NoError(t, err)
	_, err = rolePool.Exec(ctx, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s'`, appUser, appPwd))
	require.NoError(t, err)
	rolePool.Close()

	// Migrations
	db, err := sql.Open("pgx", adminURL)
	require.NoError(t, err)
	goose.SetBaseFS(migrations.FS)
	require.NoError(t, goose.SetDialect("postgres"))
	goose.SetLogger(goose.NopLogger())
	require.NoError(t, goose.Up(db, "."))
	_ = db.Close()

	// Grants pós-DDL pra nexus_app
	pool, err := pgxpool.New(ctx, adminURL)
	require.NoError(t, err)
	stmts := []string{
		`GRANT USAGE ON SCHEMA public TO nexus_app`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO nexus_app`,
		`GRANT USAGE ON ALL SEQUENCES IN SCHEMA public TO nexus_app`,
		`GRANT EXECUTE ON FUNCTION find_api_token_by_hash(TEXT) TO nexus_app`,
		`ALTER ROLE nexus_app SET search_path = public, pg_temp`,
	}
	for _, s := range stmts {
		_, err := pool.Exec(ctx, s)
		require.NoError(t, err)
	}
	pool.Close()

	appURL = strings.Replace(adminURL,
		fmt.Sprintf("%s:%s", adminUser, adminPwd),
		fmt.Sprintf("%s:%s", appUser, appPwd),
		1,
	)
	return appURL, adminURL, cleanup
}

// seedGraph cria org + entities + edges direto via admin pool (sem RLS).
// Returns: id->slug map de entities pra asserts.
func seedGraph(ctx context.Context, t *testing.T, adminURL string) (map[string]int64, time.Time) {
	t.Helper()
	pool, err := pgxpool.New(ctx, adminURL)
	require.NoError(t, err)
	defer pool.Close()

	const orgID = 1
	_, err = pool.Exec(ctx,
		`INSERT INTO organizations (id, slug, name) VALUES ($1, 'org-a', 'Org A')`, orgID)
	require.NoError(t, err)

	// Topologia (todas pessoas/conceitos, edges variadas):
	//
	//   luna ──mentions──> marte ──located_in──> ceu
	//     │                  │
	//     │                  └──happened_at──> 2020-03-14
	//     │
	//     └──works_for──> equipe (com valid_to = passado, invalidada)
	//     │
	//     └──relates_to──> conceito ──relates_to──> outro-conceito
	//
	// Past edge (valid_to = past): luna-mentions-velho-amigo (só visível com as_of antigo)

	entities := []struct{ slug, name, kind string }{
		{"luna", "Luna", "person"},
		{"marte", "Marte", "place"},
		{"ceu", "Céu", "place"},
		{"2020-03-14", "2020-03-14", "date"},
		{"equipe", "Equipe X", "organization"},
		{"conceito", "Conceito A", "concept"},
		{"outro-conceito", "Conceito B", "concept"},
		{"velho-amigo", "Velho Amigo", "person"},
	}
	idMap := make(map[string]int64)
	for _, e := range entities {
		var id int64
		err := pool.QueryRow(ctx, `
			INSERT INTO entities (organization_id, slug, name, kind, aliases, attributes)
			VALUES ($1, $2, $3, $4, '{}'::text[], '{}'::jsonb)
			RETURNING id
		`, orgID, e.slug, e.name, e.kind).Scan(&id)
		require.NoError(t, err)
		idMap[e.slug] = id
	}

	now := time.Now().UTC()
	past := now.Add(-30 * 24 * time.Hour)      // 30 dias atrás
	pastClosed := now.Add(-7 * 24 * time.Hour) // valid_to dessa edge antiga

	edges := []struct {
		from, to, kind     string
		validFrom, validTo *time.Time
		weight             float64
	}{
		// Atuais (valid_to = NULL)
		{"luna", "marte", "mentions", nil, nil, 1.0},
		{"marte", "ceu", "located_in", nil, nil, 1.0},
		{"marte", "2020-03-14", "happened_at", nil, nil, 1.0},
		{"luna", "conceito", "relates_to", nil, nil, 1.0},
		{"conceito", "outro-conceito", "relates_to", nil, nil, 0.8},
		// Edge invalidada (valid_to no passado)
		{"luna", "equipe", "works_for", &past, &pastClosed, 1.0},
		// Edge histórica (foi válida no passado mas ainda hoje? valid_to = NULL mas valid_from no passado também)
		{"luna", "velho-amigo", "mentions", &past, &pastClosed, 0.5},
	}

	for _, e := range edges {
		args := []any{orgID, idMap[e.from], idMap[e.to], e.kind, e.weight}
		query := `INSERT INTO edges (organization_id, from_entity_id, to_entity_id, kind, weight`
		valuesClause := `($1, $2, $3, $4, $5`
		if e.validFrom != nil {
			query += `, valid_from`
			valuesClause += fmt.Sprintf(`, $%d`, len(args)+1)
			args = append(args, *e.validFrom)
		}
		if e.validTo != nil {
			query += `, valid_to`
			valuesClause += fmt.Sprintf(`, $%d`, len(args)+1)
			args = append(args, *e.validTo)
		}
		query += `) VALUES ` + valuesClause + `)`
		_, err := pool.Exec(ctx, query, args...)
		require.NoErrorf(t, err, "insert edge %s→%s", e.from, e.to)
	}

	return idMap, past
}

// ───── TESTES ─────

func TestGraphTraverse_Depth1(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}
	ctx := context.Background()
	appURL, adminURL, cleanup := setupPG(ctx, t)
	defer cleanup()

	idMap, _ := seedGraph(ctx, t, adminURL)

	pool, err := pgxpool.New(ctx, appURL)
	require.NoError(t, err)
	defer pool.Close()

	hops, err := graph.Traverse(ctx, pool, 1, idMap["luna"], graph.Options{MaxDepth: 1})
	require.NoError(t, err)

	// Depth 1 = vizinhos diretos atuais: marte, conceito
	// (works_for→equipe e mentions→velho-amigo estão invalidados, não aparecem)
	slugs := make([]string, 0, len(hops))
	for _, h := range hops {
		slugs = append(slugs, h.Slug)
		assert.Equal(t, 1, h.Depth, "depth = 1 esperado em hop %s", h.Slug)
	}
	assert.ElementsMatch(t, []string{"marte", "conceito"}, slugs)
}

func TestGraphTraverse_Depth3_FanOut(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	ctx := context.Background()
	appURL, adminURL, cleanup := setupPG(ctx, t)
	defer cleanup()

	idMap, _ := seedGraph(ctx, t, adminURL)
	pool, err := pgxpool.New(ctx, appURL)
	require.NoError(t, err)
	defer pool.Close()

	hops, err := graph.Traverse(ctx, pool, 1, idMap["luna"], graph.Options{MaxDepth: 3})
	require.NoError(t, err)

	// Luna ─> marte (d1) ─> ceu (d2)
	// Luna ─> marte (d1) ─> 2020-03-14 (d2)
	// Luna ─> conceito (d1) ─> outro-conceito (d2)
	// Total esperado: 5 hops (marte, ceu, 2020-03-14, conceito, outro-conceito)
	slugs := make(map[string]int)
	for _, h := range hops {
		slugs[h.Slug] = h.Depth
	}
	assert.Len(t, hops, 5)
	assert.Equal(t, 1, slugs["marte"])
	assert.Equal(t, 1, slugs["conceito"])
	assert.Equal(t, 2, slugs["ceu"])
	assert.Equal(t, 2, slugs["2020-03-14"])
	assert.Equal(t, 2, slugs["outro-conceito"])
}

func TestGraphTraverse_KindFilter(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	ctx := context.Background()
	appURL, adminURL, cleanup := setupPG(ctx, t)
	defer cleanup()

	idMap, _ := seedGraph(ctx, t, adminURL)
	pool, err := pgxpool.New(ctx, appURL)
	require.NoError(t, err)
	defer pool.Close()

	// Só "mentions" → vai pegar luna→marte, mas marte não tem mentions saindo,
	// então deveria parar em marte (depth 1).
	hops, err := graph.Traverse(ctx, pool, 1, idMap["luna"], graph.Options{
		MaxDepth: 3,
		Kinds:    []string{"mentions"},
	})
	require.NoError(t, err)
	require.Len(t, hops, 1)
	assert.Equal(t, "marte", hops[0].Slug)
	assert.Equal(t, "mentions", hops[0].EdgeKind)
}

func TestGraphTraverse_AsOf_RevelaHistorico(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	ctx := context.Background()
	appURL, adminURL, cleanup := setupPG(ctx, t)
	defer cleanup()

	idMap, pastTime := seedGraph(ctx, t, adminURL)
	pool, err := pgxpool.New(ctx, appURL)
	require.NoError(t, err)
	defer pool.Close()

	// AsOf no passado (entre valid_from e valid_to das edges invalidadas) →
	// luna agora também aponta pra equipe e velho-amigo.
	asOf := pastTime.Add(24 * time.Hour) // depois de valid_from, antes de valid_to
	hops, err := graph.Traverse(ctx, pool, 1, idMap["luna"], graph.Options{
		MaxDepth: 1,
		AsOf:     &asOf,
	})
	require.NoError(t, err)

	slugs := make([]string, 0, len(hops))
	for _, h := range hops {
		slugs = append(slugs, h.Slug)
	}
	// Com as_of no passado: pega equipe + velho-amigo (que naquele tempo eram válidos).
	// marte e conceito têm valid_from=NULL (= default now), então NÃO estavam válidos no passado.
	assert.Contains(t, slugs, "equipe", "as_of histórico revela edge invalidada")
	assert.Contains(t, slugs, "velho-amigo", "as_of histórico revela edge invalidada")
	assert.NotContains(t, slugs, "marte", "as_of antes da edge ser criada: não aparece")
}

func TestGraphTraverse_MaxDepthCapped(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	ctx := context.Background()
	appURL, adminURL, cleanup := setupPG(ctx, t)
	defer cleanup()

	idMap, _ := seedGraph(ctx, t, adminURL)
	pool, err := pgxpool.New(ctx, appURL)
	require.NoError(t, err)
	defer pool.Close()

	// MaxDepth absurdo é capado em MaxAllowedDepth (5)
	hops, err := graph.Traverse(ctx, pool, 1, idMap["luna"], graph.Options{MaxDepth: 999})
	require.NoError(t, err)
	for _, h := range hops {
		assert.LessOrEqual(t, h.Depth, graph.MaxAllowedDepth)
	}
}

func TestGraphTraverse_EmptyForUnknownEntity(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	ctx := context.Background()
	appURL, adminURL, cleanup := setupPG(ctx, t)
	defer cleanup()

	_, _ = seedGraph(ctx, t, adminURL)
	pool, err := pgxpool.New(ctx, appURL)
	require.NoError(t, err)
	defer pool.Close()

	hops, err := graph.Traverse(ctx, pool, 1, 999999, graph.Options{MaxDepth: 3})
	require.NoError(t, err)
	assert.Empty(t, hops)
}

// EntityIDFromSlug helper
func TestEntityIDFromSlug(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	ctx := context.Background()
	appURL, adminURL, cleanup := setupPG(ctx, t)
	defer cleanup()

	idMap, _ := seedGraph(ctx, t, adminURL)
	pool, err := pgxpool.New(ctx, appURL)
	require.NoError(t, err)
	defer pool.Close()

	got, err := graph.EntityIDFromSlug(ctx, pool, 1, "luna")
	require.NoError(t, err)
	assert.Equal(t, idMap["luna"], got)

	_, err = graph.EntityIDFromSlug(ctx, pool, 1, "nao-existe")
	require.Error(t, err)
	assert.True(t, isNoRows(err))
}

func isNoRows(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "no rows") || err == pgx.ErrNoRows)
}
