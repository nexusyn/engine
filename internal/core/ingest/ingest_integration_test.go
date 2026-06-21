//go:build integration

// ingest_integration_test.go — Day 10 da Week 2 (Gate do pipeline de ingest).
//
// Cobertura end-to-end do pipeline:
//  1. IngestJob enqueued via River
//  2. IngestWorker → cria page, chunka, insere chunks (embedding=NULL)
//  3. EmbedBatchWorker (com fake provider) → preenche embedding dos chunks
//  4. Search híbrido (FTS + vector) retorna a página ingerida
//
// Estratégia: provider de embedding é stub determinístico (hash → vetor 1024).
// Sem dependência de rede externa. River roda in-process (Start/Stop).
// Aguardamos a chegada do estado final via polling com testify.assert.Eventually
// — testa comportamento observável (chunks com embedding) sem amarrar nos
// internals do River.
//
// Rodar: make test-integration  OU  go test -tags=integration ./internal/core/ingest/...
package ingest_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"strings"
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
// Usado nos tests pra eliminar dependência de rede externa.
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

// fakeLLM retorna JSON canônico ignorando o prompt — usado nos tests de
// extração de entities pra eliminar dependência de provider real.
type fakeLLM struct{ response string }

func (f *fakeLLM) Complete(_ context.Context, _ llm.Prompt) (llm.Result, error) {
	return llm.Result{Content: f.response, Provider: "fake-llm", Model: "stub"}, nil
}
func (*fakeLLM) Name() string  { return "fake-llm" }
func (*fakeLLM) Model() string { return "stub" }

// setupPostgres sobe Postgres efêmero, aplica migrations + River migrations,
// cria role nexus_app non-superuser. Retorna URLs admin/app e cleanup func.
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

// seedOrg cria a org de teste como admin (RLS não se aplica a admin).
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

// TestIngest_EndToEnd valida o pipeline completo:
// POST /v1/ingest → IngestJob → page+chunks → EmbedBatch → search retorna.
func TestIngest_EndToEnd(t *testing.T) {
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

	// adminPool é usado APENAS pra assertions cross-tenant (vê tudo, sem bind).
	// EmbedBatchWorker passa a rodar como nexus_app (RLS ativa) via SECURITY
	// DEFINER functions drain_pending_chunks + update_chunk_embedding (Day 14).
	adminPool, err := pgxpool.New(ctx, adminURL)
	require.NoError(t, err)
	defer adminPool.Close()

	// Workers in-process com fake embedder + fake LLM determinístico
	llmStub := &fakeLLM{response: `{
		"entities": [
			{"name": "Luna", "kind": "person", "aliases": ["gata Luna"], "attributes": {"weight_kg": 4.2}},
			{"name": "2020-03-14", "kind": "date"}
		],
		"edges": [
			{"from_name": "Luna", "to_name": "2020-03-14", "kind": "happened_at"}
		]
	}`}
	workers := river.NewWorkers()
	require.NoError(t, job.RegisterCoreWorkers(workers, pool))
	require.NoError(t, job.RegisterEmbedWorker(workers, pool, &fakeEmbedder{dim: embedDims}, nil, 50))
	require.NoError(t, job.RegisterExtractEntitiesWorker(workers, pool, llmStub, nil))

	client, err := job.NewClient(pool, workers)
	require.NoError(t, err)
	require.NoError(t, client.Start(ctx))
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = client.Stop(stopCtx)
	}()

	// Enfileira IngestJob (mesmo caminho do POST /v1/ingest)
	insertClient, err := job.NewInsertOnlyClient(pool)
	require.NoError(t, err)

	content := strings.Join([]string{
		"Linha 1: o gato Luna come ração de salmão preferida da casa.",
		"Linha 2: aniversário da Luna é em 2020-03-14, gato laranja de 4 anos.",
		"Linha 3: Luna pesa 4.2kg e adora dormir no parapeito da janela.",
	}, "\n\n")

	_, err = insertClient.Insert(ctx, job.IngestArgs{
		OrganizationID: orgID,
		Title:          "Memory test page Luna",
		Content:        content,
		Domain:         "memory",
	}, &river.InsertOpts{})
	require.NoError(t, err)

	// Aguarda pipeline drenar: chunks com embedding E entities extraídas.
	// Reusa adminPool criado acima (BYPASSRLS pra assertion sem precisar de bind).
	require.Eventually(t, func() bool {
		var totalChunks, withEmbed, pagesProcessed int
		if err := adminPool.QueryRow(ctx,
			`SELECT COUNT(*), COUNT(*) FILTER (WHERE embedding IS NOT NULL) FROM chunks WHERE organization_id = $1`,
			orgID,
		).Scan(&totalChunks, &withEmbed); err != nil {
			return false
		}
		if err := adminPool.QueryRow(ctx,
			`SELECT COUNT(*) FROM pages WHERE organization_id = $1 AND entities_extracted_at IS NOT NULL`, orgID,
		).Scan(&pagesProcessed); err != nil {
			return false
		}
		return totalChunks > 0 && totalChunks == withEmbed && pagesProcessed > 0
	}, 30*time.Second, 250*time.Millisecond, "esperando IngestJob + EmbedBatchJob + ExtractEntitiesJob drenarem")

	// ───── Asserts de página ─────
	var pageCount int
	var pageTitle, pageSlug, pageDomain string
	require.NoError(t, adminPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM pages WHERE organization_id = $1`, orgID).Scan(&pageCount))
	assert.Equal(t, 1, pageCount, "uma page criada")

	require.NoError(t, adminPool.QueryRow(ctx,
		`SELECT title, slug, domain FROM pages WHERE organization_id = $1`, orgID,
	).Scan(&pageTitle, &pageSlug, &pageDomain))
	assert.Equal(t, "Memory test page Luna", pageTitle)
	// Slug tem sufixo random (`-XXXXXX`) por design — match por prefix.
	assert.True(t, strings.HasPrefix(pageSlug, "memory-test-page-luna-"), "slug prefix: %s", pageSlug)
	assert.Equal(t, "memory", pageDomain)

	// ───── Asserts de chunks ─────
	var chunkCount int
	require.NoError(t, adminPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM chunks WHERE organization_id = $1`, orgID).Scan(&chunkCount))
	assert.GreaterOrEqual(t, chunkCount, 1, "pelo menos um chunk criado")

	// position começa em 0 e é sequencial
	rows, err := adminPool.Query(ctx,
		`SELECT position FROM chunks WHERE organization_id = $1 ORDER BY position`, orgID)
	require.NoError(t, err)
	var positions []int
	for rows.Next() {
		var p int
		require.NoError(t, rows.Scan(&p))
		positions = append(positions, p)
	}
	rows.Close()
	require.NoError(t, rows.Err())
	for i, p := range positions {
		assert.Equal(t, i, p, "position deve ser sequencial 0..N")
	}

	// ───── Asserts de entities + edges (Day 15) ─────
	// LLM stub retornou 2 entities (Luna, 2020-03-14) + 1 edge (happened_at)
	var entityCount, edgeCount int
	require.NoError(t, adminPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM entities WHERE organization_id = $1`, orgID).Scan(&entityCount))
	assert.Equal(t, 2, entityCount, "2 entities extraídas")

	require.NoError(t, adminPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM edges WHERE organization_id = $1`, orgID).Scan(&edgeCount))
	assert.Equal(t, 1, edgeCount, "1 edge (Luna →happened_at→ 2020-03-14)")

	// Slug + aliases + attributes batem com o JSON do stub
	var lunaName, lunaKind string
	var lunaAliases []string
	require.NoError(t, adminPool.QueryRow(ctx,
		`SELECT name, kind, aliases FROM entities WHERE organization_id = $1 AND slug = 'luna'`, orgID,
	).Scan(&lunaName, &lunaKind, &lunaAliases))
	assert.Equal(t, "Luna", lunaName)
	assert.Equal(t, "person", lunaKind)
	assert.Contains(t, lunaAliases, "gata Luna")

	// ───── Smoke: search FTS retorna chunk com 'Luna' ─────
	searchSvc := search.NewService(pool, &fakeEmbedder{dim: embedDims})
	ftsResults, err := searchSvc.Search(ctx, orgID, search.Options{
		Query: "Luna ração salmão",
		Limit: 5,
		Mode:  search.ModeFTS,
	})
	require.NoError(t, err)
	assert.NotEmpty(t, ftsResults, "FTS deve retornar pelo menos 1 chunk")

	// Cobertura do bug "$$N" (Day 21): search com Domain setado
	// (caminho não exercitado nos asserts FTS/hybrid acima).
	domainResults, err := searchSvc.Search(ctx, orgID, search.Options{
		Query:  "Luna",
		Limit:  3,
		Mode:   search.ModeFTS,
		Domain: "memory",
	})
	require.NoError(t, err, "search com Domain não deve dar erro SQL ($$N regressão)")
	assert.NotEmpty(t, domainResults, "Domain=memory filtra mas Luna é dessa domain")
	if len(ftsResults) > 0 {
		assert.Contains(t, strings.ToLower(ftsResults[0].Content), "luna",
			"FTS retorna o chunk certo")
		assert.True(t, strings.HasPrefix(ftsResults[0].PageSlug, "memory-test-page-luna-"),
			"slug do top result: %s", ftsResults[0].PageSlug)
	}

	// ───── Smoke: search hybrid (vector + FTS via RRF) ─────
	hybridResults, err := searchSvc.Search(ctx, orgID, search.Options{
		Query: "Luna",
		Limit: 5,
	})
	require.NoError(t, err)
	assert.NotEmpty(t, hybridResults, "hybrid search deve retornar pelo menos 1 chunk")
	if len(hybridResults) > 0 {
		assert.Equal(t, "hybrid", hybridResults[0].Source)
	}
}

// TestIngest_MultiplePages valida que múltiplas pages drenam corretamente,
// mantendo isolamento entre elas (mesma org, pages diferentes).
func TestIngest_MultiplePages(t *testing.T) {
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

	// adminPool só pra assertions; worker usa pool nexus_app via SECURITY DEFINER.
	adminPool, err := pgxpool.New(ctx, adminURL)
	require.NoError(t, err)
	defer adminPool.Close()

	workers := river.NewWorkers()
	llmStub := &fakeLLM{response: `{"entities":[],"edges":[]}`}
	require.NoError(t, job.RegisterCoreWorkers(workers, pool))
	require.NoError(t, job.RegisterEmbedWorker(workers, pool, &fakeEmbedder{dim: embedDims}, nil, 50))
	require.NoError(t, job.RegisterExtractEntitiesWorker(workers, pool, llmStub, nil))

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

	pages := []job.IngestArgs{
		{OrganizationID: orgID, Title: "Receita de bolo", Content: "Misture farinha, ovos e açúcar. Asse a 180C.", Domain: "memory"},
		{OrganizationID: orgID, Title: "Lista de compras", Content: "Comprar leite, pão e queijo no mercado.", Domain: "memory"},
		{OrganizationID: orgID, Title: "Reunião quinta", Content: "Apresentar protótipo do NEXUS pro time.", Domain: "memory"},
	}
	for _, p := range pages {
		_, err := insertClient.Insert(ctx, p, &river.InsertOpts{})
		require.NoError(t, err)
	}

	require.Eventually(t, func() bool {
		var pageCount, chunkCount, withEmbed int
		if err := adminPool.QueryRow(ctx,
			`SELECT COUNT(*) FROM pages WHERE organization_id = $1`, orgID,
		).Scan(&pageCount); err != nil {
			return false
		}
		if err := adminPool.QueryRow(ctx,
			`SELECT COUNT(*), COUNT(*) FILTER (WHERE embedding IS NOT NULL) FROM chunks WHERE organization_id = $1`,
			orgID,
		).Scan(&chunkCount, &withEmbed); err != nil {
			return false
		}
		return pageCount == len(pages) && chunkCount > 0 && chunkCount == withEmbed
	}, 30*time.Second, 250*time.Millisecond, "esperando %d pages + chunks com embedding", len(pages))

	// Search por termo específico de cada page volta a page certa.
	searchSvc := search.NewService(pool, &fakeEmbedder{dim: embedDims})
	// Cada query usa termos PRESENTES NO CONTEÚDO do chunk (não no título —
	// FTS só indexa chunks.content). `plainto_tsquery` faz AND, todos termos
	// precisam estar na mesma chunk.
	cases := []struct {
		query    string
		slugPref string
		wantHint string
	}{
		{"farinha açúcar", "receita-de-bolo-", "termos da receita"},
		{"queijo leite", "lista-de-compras-", "termos da lista"},
		{"protótipo NEXUS", "reuniao-quinta-", "termos da reunião"},
	}
	for _, tc := range cases {
		t.Run(tc.slugPref, func(t *testing.T) {
			results, err := searchSvc.Search(ctx, orgID, search.Options{
				Query: tc.query,
				Limit: 3,
				Mode:  search.ModeFTS,
			})
			require.NoError(t, err)
			require.NotEmpty(t, results, tc.wantHint)
			assert.True(t, strings.HasPrefix(results[0].PageSlug, tc.slugPref),
				"%s: esperava slug com prefix %q, veio %q", tc.wantHint, tc.slugPref, results[0].PageSlug)
		})
	}
}
