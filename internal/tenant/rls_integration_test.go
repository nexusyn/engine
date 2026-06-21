//go:build integration

// rls_integration_test.go — GATE da Semana 1.
//
// Este teste prova que RLS (Row-Level Security) bloqueia cross-tenant leak.
// Se este teste falhar, NÃO promovemos NEXUS pra produção.
//
// Mecânica:
//  1. Sobe Postgres efêmero via testcontainers (imagem pgvector pra simplicidade)
//  2. Aplica todas migrations (incluindo 0008_rls_policies)
//  3. Cria role `nexus_app` (non-superuser, sem BYPASSRLS) — essencial pra RLS valer
//  4. Conecta como nexus_app no pool de teste
//  5. Insere dados pra 2 orgs (Luna e Mittens — cat names)
//  6. Valida:
//     - Sem tenant bind → 0 rows visíveis (RLS esconde tudo)
//     - Com tenant = 1 → vê só Luna
//     - Com tenant = 2 → vê só Mittens
//     - Após COMMIT, próximo query sem bind → 0 rows (SET LOCAL não vaza)
//
// Rodar: make test-rls  OU  go test -tags=integration -run TestRLS ./internal/tenant/...
package tenant_test

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

	"github.com/nexusyn/engine/internal/tenant"
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

// setupPostgres sobe um container Postgres com a imagem pgvector,
// aplica migrations e cria a role nexus_app.
// Retorna a connection string como nexus_app (non-superuser).
func setupPostgres(ctx context.Context, t *testing.T) (string, func()) {
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

	cleanup := func() {
		// Best-effort cleanup
		_ = pgContainer.Terminate(context.Background())
	}

	adminURL, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	// 1) Criar role nexus_app ANTES das migrations — espelha docker/postgres-init.sh
	//    (0020+ fazem GRANT incondicional a nexus_app e falham sem a role)
	createAppRole(t, ctx, adminURL)

	// 2) Aplicar migrations como admin (usa nossa embed.FS)
	applyMigrations(t, adminURL)

	// 3) Grants pós-DDL pra nexus_app (non-superuser, non-BYPASSRLS)
	createAppUser(t, ctx, adminURL)

	// 3) Substituir credentials no URL para connect como app user
	appURL := strings.Replace(adminURL,
		fmt.Sprintf("%s:%s", adminUser, adminPwd),
		fmt.Sprintf("%s:%s", appUser, appPwd),
		1,
	)
	return appURL, cleanup
}

func applyMigrations(t *testing.T, dbURL string) {
	t.Helper()

	db, err := sql.Open("pgx", dbURL)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	goose.SetBaseFS(migrations.FS)
	require.NoError(t, goose.SetDialect("postgres"))
	goose.SetLogger(goose.NopLogger())
	require.NoError(t, goose.Up(db, "."))
}

// createAppRole cria a role nexus_app ANTES das migrations — espelha o
// docker/postgres-init.sh real (role primeiro, DDL depois).
func createAppRole(t *testing.T, ctx context.Context, adminURL string) {
	t.Helper()

	pool, err := pgxpool.New(ctx, adminURL)
	require.NoError(t, err)
	defer pool.Close()
	_, err = pool.Exec(ctx, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s'`, appUser, appPwd))
	require.NoError(t, err)
}

func createAppUser(t *testing.T, ctx context.Context, adminURL string) {
	t.Helper()

	pool, err := pgxpool.New(ctx, adminURL)
	require.NoError(t, err)
	defer pool.Close()

	stmts := []string{
		// Grants pra DML em todas tabelas existentes
		`GRANT USAGE ON SCHEMA public TO nexus_app`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO nexus_app`,
		`GRANT USAGE ON ALL SEQUENCES IN SCHEMA public TO nexus_app`,
		// Permite chamar a função SECURITY DEFINER do auth
		`GRANT EXECUTE ON FUNCTION find_api_token_by_hash(TEXT) TO nexus_app`,
		// Bind config customizada (necessário pra SET LOCAL nexus.org_id)
		`ALTER ROLE nexus_app SET search_path = public, pg_temp`,
	}
	for _, s := range stmts {
		_, err := pool.Exec(ctx, s)
		require.NoErrorf(t, err, "stmt: %s", s)
	}
}

func seedOrgsAndData(t *testing.T, ctx context.Context, adminURL string) {
	t.Helper()

	pool, err := pgxpool.New(ctx, adminURL)
	require.NoError(t, err)
	defer pool.Close()

	// Insere 2 orgs e 2 pages (uma por org). Como admin, RLS não se aplica.
	stmts := []string{
		`INSERT INTO organizations (id, slug, name) VALUES (1, 'org-a', 'Org A')`,
		`INSERT INTO organizations (id, slug, name) VALUES (2, 'org-b', 'Org B')`,
		`INSERT INTO pages (organization_id, slug, title, content, domain) VALUES (1, 'cat', 'Cat', 'Luna', 'memory')`,
		`INSERT INTO pages (organization_id, slug, title, content, domain) VALUES (2, 'cat', 'Cat', 'Mittens', 'memory')`,
		// Reseta sequence pra próximo INSERT não conflitar
		`SELECT setval('organizations_id_seq', 2, true)`,
	}
	for _, s := range stmts {
		_, err := pool.Exec(ctx, s)
		require.NoErrorf(t, err, "seed stmt: %s", s)
	}
}

// ───── TESTES PRINCIPAIS ─────

// TestRLS_CrossTenantIsolation é o gate da Semana 1.
//
// Cenários:
//  1. App user SEM bind → query retorna 0 rows (RLS bloqueia)
//  2. Bind tenant 1 via RunWithTenant → vê só pages da org 1
//  3. Bind tenant 2 → vê só pages da org 2
//  4. Após COMMIT da tx, query sem bind volta a ver 0 → SET LOCAL não vazou
func TestRLS_CrossTenantIsolation(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}

	ctx := context.Background()

	appURL, cleanup := setupPostgres(ctx, t)
	defer cleanup()

	// Admin URL pra seed (volta as creds do admin)
	adminURL := strings.Replace(appURL,
		fmt.Sprintf("%s:%s", appUser, appPwd),
		fmt.Sprintf("%s:%s", adminUser, adminPwd),
		1,
	)
	seedOrgsAndData(t, ctx, adminURL)

	// Connect como nexus_app (non-superuser, RLS ATIVA)
	pool, err := pgxpool.New(ctx, appURL)
	require.NoError(t, err, "pool app user")
	defer pool.Close()

	// ───── CENÁRIO 1: sem tenant bind → 0 rows ─────
	t.Run("sem_tenant_bind_zero_rows", func(t *testing.T) {
		var count int
		err := pool.QueryRow(ctx, "SELECT count(*) FROM pages").Scan(&count)
		require.NoError(t, err)
		assert.Equal(t, 0, count,
			"RLS deve esconder todos os rows quando nexus.org_id não está setado")
	})

	// ───── CENÁRIO 2: bind tenant 1 → só Luna ─────
	t.Run("tenant_1_ve_apenas_Luna", func(t *testing.T) {
		var content string
		var count int
		err := tenant.RunWithTenant(ctx, pool, 1, func(tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, "SELECT count(*) FROM pages").Scan(&count); err != nil {
				return err
			}
			return tx.QueryRow(ctx, "SELECT content FROM pages").Scan(&content)
		})
		require.NoError(t, err)
		assert.Equal(t, 1, count, "tenant 1 deve ver exatamente 1 row")
		assert.Equal(t, "Luna", content)
	})

	// ───── CENÁRIO 3: bind tenant 2 → só Mittens ─────
	t.Run("tenant_2_ve_apenas_Mittens", func(t *testing.T) {
		var content string
		var count int
		err := tenant.RunWithTenant(ctx, pool, 2, func(tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, "SELECT count(*) FROM pages").Scan(&count); err != nil {
				return err
			}
			return tx.QueryRow(ctx, "SELECT content FROM pages").Scan(&content)
		})
		require.NoError(t, err)
		assert.Equal(t, 1, count)
		assert.Equal(t, "Mittens", content)
	})

	// ───── CENÁRIO 4: após COMMIT, query sem bind volta a ver 0 ─────
	t.Run("set_local_nao_vaza_apos_commit", func(t *testing.T) {
		// Após o RunWithTenant do cenário 3, o nexus.org_id foi descartado.
		// Próxima query no mesmo pool (sem nova tx com SET LOCAL) deve ver 0 rows.
		var count int
		err := pool.QueryRow(ctx, "SELECT count(*) FROM pages").Scan(&count)
		require.NoError(t, err)
		assert.Equal(t, 0, count,
			"SET LOCAL deve ter sido descartado no COMMIT — sem vazamento")
	})

	// ───── CENÁRIO 5: INSERT cross-tenant é BLOQUEADO (Day 21 — migration 0013) ─────
	t.Run("insert_cross_tenant_bloqueado_por_with_check", func(t *testing.T) {
		// Bind como tenant 1, tenta inserir page declarando organization_id=2
		err := tenant.RunWithTenant(ctx, pool, 1, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				"INSERT INTO pages (organization_id, slug, title, content, domain) VALUES (2, 'mal', 'm', 'm', 'memory')")
			return err
		})
		// Migration 0013 adicionou WITH CHECK em todas policies tenant_isolation.
		// Agora o INSERT falha imediatamente — não vira row órfã/invisível.
		require.Error(t, err, "INSERT cross-tenant deve falhar com policy violation")
		assert.Contains(t, err.Error(), "row-level security",
			"erro deve mencionar RLS (não outro erro)")
	})

	// ───── CENÁRIO 6: tentativa de SET sem LOCAL ─────
	// (esse cenário é defensivo: se algum dia o RunWithTenant for substituído por
	// código que esquece o LOCAL, o pool vai vazar. Documentamos como prevenir.)
	t.Run("set_sem_local_vaza_no_pool", func(t *testing.T) {
		// 1ª connection
		conn, err := pool.Acquire(ctx)
		require.NoError(t, err)
		_, err = conn.Exec(ctx, "SET nexus.org_id = 1") // BUG: sem LOCAL
		require.NoError(t, err)
		conn.Release()

		// 2ª query reusa a mesma connection — herda nexus.org_id=1
		var count int
		err = pool.QueryRow(ctx, "SELECT count(*) FROM pages").Scan(&count)
		require.NoError(t, err)

		// Pode ou não vazar dependendo de qual conn vier do pool (pool de 10+)
		// Documentamos comportamento mas não falhamos teste — depende de pool size.
		// O importante é mostrar que NÃO se faz SET sem LOCAL.
		t.Logf("count após SET sem LOCAL: %d (esperado vazar pra 1 se mesma conn)", count)

		// Limpa pra próximos testes não serem afetados
		_, _ = pool.Exec(ctx, "RESET nexus.org_id")
	})
}

// TestRunWithTenant_InvalidTenantID valida que tenant inválido (0, negativo) é rejeitado
// SEM tocar no DB — fail fast.
func TestRunWithTenant_InvalidTenantID(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}

	ctx := context.Background()
	appURL, cleanup := setupPostgres(ctx, t)
	defer cleanup()

	pool, err := pgxpool.New(ctx, appURL)
	require.NoError(t, err)
	defer pool.Close()

	called := false
	err = tenant.RunWithTenant(ctx, pool, 0, func(tx pgx.Tx) error {
		called = true
		return nil
	})
	assert.Error(t, err)
	assert.False(t, called, "fn não deve ser chamada pra tenantID inválido")

	err = tenant.RunWithTenant(ctx, pool, -5, func(tx pgx.Tx) error {
		return nil
	})
	assert.Error(t, err)
}
