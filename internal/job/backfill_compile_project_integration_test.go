//go:build integration

// backfill_compile_project_integration_test.go — valida o backfill
// scripts/backfill_compile_project.sql: derivados globais ANTIGOS recebem o
// project das suas memórias-fonte quando TODAS compartilham um único project;
// fontes mistas (2+ projects, ou project+global) permanecem globais.
//
// A query de APPLY aqui deve espelhar o PASSO 2 do .sql (mantém em sincronia).
// Usa o pool admin (superuser do testcontainer) que bypassa RLS — espelha a role
// nexus_service (BYPASSRLS) que roda o backfill em prod.
package job

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// backfillApplySQL — PASSO 2 do scripts/backfill_compile_project.sql (sem BEGIN/
// COMMIT; o teste gerencia a própria conexão).
const backfillApplySQL = `
WITH src AS (
    SELECT d.id AS page_id, s.project AS src_project
    FROM pages d
    CROSS JOIN LATERAL jsonb_array_elements_text(d.metadata->'source_page_ids') AS sid(v)
    JOIN pages s ON s.id = sid.v::bigint AND s.organization_id = d.organization_id
    WHERE d.source_type = 'compiled' AND d.project IS NULL AND d.valid_to IS NULL
),
resolved AS (
    SELECT page_id,
           CASE WHEN count(*) = count(src_project)
                 AND count(DISTINCT src_project) = 1
                THEN max(src_project) END AS proj
    FROM src
    GROUP BY page_id
)
UPDATE pages d
SET project = r.proj
FROM resolved r
WHERE d.id = r.page_id AND r.proj IS NOT NULL`

func TestBackfillCompileProject(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	ctx := context.Background()
	appURL, adminURL, cleanup := setupPG(ctx, t)
	defer cleanup()
	_ = appURL
	org := seedOrg(ctx, t, adminURL)

	pool, err := pgxpool.New(ctx, adminURL) // superuser → bypassa RLS (espelha nexus_service)
	require.NoError(t, err)
	defer pool.Close()

	// Fontes (memory) com project distinto + uma global.
	mNexus := insertSource(ctx, t, pool, org, "src-nexus", "nexusyn")
	mReach := insertSource(ctx, t, pool, org, "src-reach", "reachyn")
	mGlobal := insertSource(ctx, t, pool, org, "src-global", "")

	// Derivados ANTIGOS (compiled, project NULL) com proveniências variadas.
	dPuro := insertDerived(ctx, t, pool, org, "lesson-puro", []int64{mNexus})            // 1 project → nexusyn
	dMisto := insertDerived(ctx, t, pool, org, "lesson-misto", []int64{mNexus, mReach})  // 2 projects → global
	dProjGlobal := insertDerived(ctx, t, pool, org, "lesson-pg", []int64{mNexus, mGlobal}) // project+global → global
	dSoGlobal := insertDerived(ctx, t, pool, org, "lesson-glob", []int64{mGlobal})        // só global → global

	// GUARDA (PASSO 0): nenhuma página viva duplicada por (org, domain, slug).
	var dups int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM (
			SELECT 1 FROM pages WHERE valid_to IS NULL
			GROUP BY organization_id, domain, slug HAVING count(*) > 1
		) t`).Scan(&dups))
	require.Equal(t, 0, dups, "guarda PASSO 0 deve achar 0 duplicatas vivas")

	// APPLY.
	_, err = pool.Exec(ctx, backfillApplySQL)
	require.NoError(t, err)

	assert.Equal(t, "nexusyn", projectOf(ctx, t, pool, dPuro), "fonte única nexusyn → derivado vira nexusyn")
	assert.Equal(t, "", projectOf(ctx, t, pool, dMisto), "2 projects → permanece global")
	assert.Equal(t, "", projectOf(ctx, t, pool, dProjGlobal), "project+global → permanece global")
	assert.Equal(t, "", projectOf(ctx, t, pool, dSoGlobal), "só global → permanece global")

	// Idempotente: rodar de novo não muda nada.
	_, err = pool.Exec(ctx, backfillApplySQL)
	require.NoError(t, err)
	assert.Equal(t, "nexusyn", projectOf(ctx, t, pool, dPuro))
}

func insertSource(ctx context.Context, t *testing.T, pool *pgxpool.Pool, org int64, slug, project string) int64 {
	t.Helper()
	var id int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO pages (organization_id, slug, title, content, domain, project, source_type, metadata)
		 VALUES ($1, $2, $2, 'conteudo da fonte', 'memory', NULLIF($3,''), 'ingested', '{}'::jsonb) RETURNING id`,
		org, slug, project).Scan(&id))
	return id
}

func insertDerived(ctx context.Context, t *testing.T, pool *pgxpool.Pool, org int64, slug string, sourceIDs []int64) int64 {
	t.Helper()
	var id int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO pages (organization_id, slug, title, content, domain, project, source_type, metadata)
		 VALUES ($1, $2, $2, 'derivado', 'lesson', NULL, 'compiled',
		         jsonb_build_object('compiled', true, 'source_page_ids', $3::bigint[])) RETURNING id`,
		org, slug, sourceIDs).Scan(&id))
	return id
}

func projectOf(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id int64) string {
	t.Helper()
	var p *string
	require.NoError(t, pool.QueryRow(ctx, `SELECT project FROM pages WHERE id=$1`, id).Scan(&p))
	if p == nil {
		return ""
	}
	return *p
}
