//go:build integration

// roundtrip_integration_test.go — Memory round-trip end-to-end.
//
// Cenário: usuário fala um fato → algum tempo depois pergunta sobre o fato
// → NEXUS responde citando-o. Testa o pipeline INTEIRO sem mock no meio:
//
//	ingest → chunk → embed → store → search híbrido → LLM grounded
//
// Provider de embedding é stub determinístico (sem rede).
// LLM é "capturador" que devolve resposta fixa mas grava o prompt recebido —
// nos asserts validamos que o chunk com o fato chegou ao prompt, provando que
// a memória foi recuperada corretamente.
//
// Rodar: make test-integration  OU  go test -tags=integration ./internal/core/query/...
package query_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/nexusyn/engine/internal/core/query"
	"github.com/nexusyn/engine/internal/core/search"
	"github.com/nexusyn/engine/internal/job"
	"github.com/nexusyn/engine/internal/provider/embed"
	"github.com/nexusyn/engine/internal/provider/llm"
	"github.com/nexusyn/engine/migrations"
)

const (
	pgImage   = "pgvector/pgvector:pg17"
	dbName    = "nexus_test"
	adminUser = "nexus_admin"
	adminPwd  = "admin-test"
	appUser   = "nexus_app"
	appPwd    = "app-test"
	embedDims = 1024
)

// fakeEmbedder produz vetores determinísticos via SHA-256 do texto.
type fakeEmbedder struct{ dim int }

func (f *fakeEmbedder) Embed(_ context.Context, texts []string, _ embed.InputType) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		h := sha256.Sum256([]byte(t))
		v := make([]float32, f.dim)
		for j := range v {
			v[j] = float32(h[j%len(h)]) / 255.0
		}
		out[i] = v
	}
	return out, nil
}
func (f *fakeEmbedder) Name() string  { return "fake-sha256" }
func (f *fakeEmbedder) Model() string { return "deterministic-hash" }
func (f *fakeEmbedder) Dim() int      { return f.dim }

// capturingLLM grava o último prompt recebido e devolve resposta fixa.
type capturingLLM struct {
	mu         sync.Mutex
	response   string
	lastPrompt llm.Prompt
	callCount  int
}

func (c *capturingLLM) Complete(_ context.Context, p llm.Prompt) (llm.Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastPrompt = p
	c.callCount++
	return llm.Result{Content: c.response, Provider: "fake-llm", Model: "stub"}, nil
}
func (*capturingLLM) Name() string  { return "fake-llm" }
func (*capturingLLM) Model() string { return "stub" }

func (c *capturingLLM) LastPrompt() llm.Prompt {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastPrompt
}

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

	cleanup = func() {
		_ = pgContainer.Terminate(context.Background())
	}

	adminURL, err = pgContainer.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	createAppRole(ctx, t, adminURL) // ANTES das migrations (0020+ têm GRANTs à role)
	applyGooseMigrations(t, adminURL)
	applyRiverMigrations(ctx, t, adminURL)
	createAppUser(ctx, t, adminURL)

	appURL = strings.Replace(adminURL,
		fmt.Sprintf("%s:%s", adminUser, adminPwd),
		fmt.Sprintf("%s:%s", appUser, appPwd),
		1,
	)
	return appURL, adminURL, cleanup
}

func applyGooseMigrations(t *testing.T, dbURL string) {
	t.Helper()
	db, err := sql.Open("pgx", dbURL)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	goose.SetBaseFS(migrations.FS)
	require.NoError(t, goose.SetDialect("postgres"))
	goose.SetLogger(goose.NopLogger())
	require.NoError(t, goose.Up(db, "."))
}

func applyRiverMigrations(ctx context.Context, t *testing.T, adminURL string) {
	t.Helper()
	pool, err := pgxpool.New(ctx, adminURL)
	require.NoError(t, err)
	defer pool.Close()
	require.NoError(t, job.MigrateUp(ctx, pool))
}

// createAppRole cria a role nexus_app ANTES das migrations — espelha o
// docker/postgres-init.sh real (role primeiro, DDL depois). Migrations 0020+
// fazem GRANT incondicional a nexus_app e falham em DB fresco sem a role.
func createAppRole(ctx context.Context, t *testing.T, adminURL string) {
	t.Helper()
	pool, err := pgxpool.New(ctx, adminURL)
	require.NoError(t, err)
	defer pool.Close()
	_, err = pool.Exec(ctx, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s'`, appUser, appPwd))
	require.NoError(t, err)
}

func createAppUser(ctx context.Context, t *testing.T, adminURL string) {
	t.Helper()
	pool, err := pgxpool.New(ctx, adminURL)
	require.NoError(t, err)
	defer pool.Close()

	stmts := []string{
		`GRANT USAGE ON SCHEMA public TO nexus_app`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO nexus_app`,
		`GRANT USAGE ON ALL SEQUENCES IN SCHEMA public TO nexus_app`,
		`GRANT EXECUTE ON FUNCTION find_api_token_by_hash(TEXT) TO nexus_app`,
		`ALTER ROLE nexus_app SET search_path = public, pg_temp`,
	}
	for _, s := range stmts {
		_, err := pool.Exec(ctx, s)
		require.NoErrorf(t, err, "stmt: %s", s)
	}
}

func seedOrg(ctx context.Context, t *testing.T, adminURL string, orgID int64) {
	t.Helper()
	pool, err := pgxpool.New(ctx, adminURL)
	require.NoError(t, err)
	defer pool.Close()

	_, err = pool.Exec(ctx,
		`INSERT INTO organizations (id, slug, name) VALUES ($1, $2, $3)`,
		orgID, fmt.Sprintf("org-%d", orgID), fmt.Sprintf("Org %d", orgID),
	)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `SELECT setval('organizations_id_seq', GREATEST($1, 1), true)`, orgID)
	require.NoError(t, err)
}

// TestMemoryRoundTrip — gate funcional da memória.
//
// 1. Ingere um fato sobre o usuário.
// 2. Pergunta sobre o fato.
// 3. Confirma que o chunk com o fato chegou ao prompt do LLM (memória recuperada).
func TestMemoryRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}

	ctx := context.Background()
	appURL, adminURL, cleanup := setupPostgres(ctx, t)
	defer cleanup()

	const orgID int64 = 1
	seedOrg(ctx, t, adminURL, orgID)

	pool, err := pgxpool.New(ctx, appURL)
	require.NoError(t, err)
	defer pool.Close()

	adminPool, err := pgxpool.New(ctx, adminURL)
	require.NoError(t, err)
	defer adminPool.Close()

	// Workers in-process pro pipeline de ingest.
	workers := river.NewWorkers()
	entitiesLLM := &capturingLLM{response: `{"entities":[],"edges":[]}`}
	require.NoError(t, job.RegisterCoreWorkers(workers, pool))
	require.NoError(t, job.RegisterEmbedWorker(workers, pool, &fakeEmbedder{dim: embedDims}, nil, 50))
	require.NoError(t, job.RegisterExtractEntitiesWorker(workers, pool, entitiesLLM, nil))

	client, err := job.NewClient(pool, workers)
	require.NoError(t, err)
	require.NoError(t, client.Start(ctx))
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = client.Stop(stopCtx)
	}()

	insertClient, err := job.NewInsertOnlyClient(pool)
	require.NoError(t, err)

	// ───── Passo 1: ingere o fato ─────
	const fact = "Luciano prefere Postgres como banco de dados principal do NEXUS v2. " +
		"MongoDB foi descartado. Postgres com pgvectorscale é o storage default em produção."

	_, err = insertClient.Insert(ctx, job.IngestArgs{
		OrganizationID: orgID,
		Title:          "Preferências Luciano",
		Content:        fact,
		Domain:         "memory",
	}, &river.InsertOpts{})
	require.NoError(t, err)

	// Aguarda pipeline drenar: chunk + embedding presentes.
	require.Eventually(t, func() bool {
		var totalChunks, withEmbed int
		if err := adminPool.QueryRow(ctx,
			`SELECT COUNT(*), COUNT(*) FILTER (WHERE embedding IS NOT NULL) FROM chunks WHERE organization_id = $1`,
			orgID,
		).Scan(&totalChunks, &withEmbed); err != nil {
			return false
		}
		return totalChunks > 0 && totalChunks == withEmbed
	}, 30*time.Second, 250*time.Millisecond, "esperando chunks com embedding")

	// ───── Passo 2: pergunta sobre o fato ─────
	queryLLM := &capturingLLM{response: "Postgres com pgvectorscale."}
	searchSvc := search.NewService(pool, &fakeEmbedder{dim: embedDims})
	querySvc := query.NewServiceWithPool(searchSvc, queryLLM, nil, pool)

	resp, err := querySvc.Query(ctx, orgID, query.Options{
		Question: "Qual banco Luciano prefere?",
		Limit:    5,
		// Mode default (hybrid) — vector com fakeEmbedder sempre retorna top-K
		// e FTS pega "banco" + "Luciano" + "prefere" (todos presentes no chunk).
	})
	require.NoError(t, err)

	// ───── Asserts: memória recuperada e injetada no prompt ─────
	require.NotEmpty(t, resp.Sources, "search deve retornar pelo menos 1 fonte")

	foundInSources := false
	for _, s := range resp.Sources {
		if strings.Contains(strings.ToLower(s.Content), "postgres") {
			foundInSources = true
			break
		}
	}
	assert.True(t, foundInSources, "alguma source recuperada contém 'postgres'")

	prompt := queryLLM.LastPrompt()
	assert.Contains(t, strings.ToLower(prompt.User), "postgres",
		"prompt enviado ao LLM contém o fato — prova que ingest→embed→search→retrieve chegou ao LLM")

	assert.Contains(t, strings.ToLower(prompt.User), "luciano",
		"contexto do prompt preserva o sujeito do fato")

	assert.NotEmpty(t, resp.Answer, "LLM retorna alguma resposta")
	assert.Equal(t, 1, queryLLM.callCount, "LLM chamado exatamente 1 vez (sem MultiHop)")
}
