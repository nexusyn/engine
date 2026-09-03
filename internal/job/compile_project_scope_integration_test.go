//go:build integration

// compile_project_scope_integration_test.go — o compile grava o `project` nas
// páginas DERIVADAS (herdado das memórias-fonte) e escopa o dedup por projeto.
// Fecha o vazamento onde o filtro `project=X` da busca trazia lesson/wiki de
// OUTROS projetos (todo derivado nascia global/NULL).
//
// Reusa setupPG/seedOrg/countCurrentPages de ingest_dedup_integration_test.go
// (mesma package + tag integration).
package job

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexusyn/engine/internal/core/compile"
)

func TestPersistPages_ProjectScoping(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	ctx := context.Background()
	appURL, adminURL, cleanup := setupPG(ctx, t)
	defer cleanup()
	org := seedOrg(ctx, t, adminURL)

	pool, err := pgxpool.New(ctx, appURL)
	require.NoError(t, err)
	defer pool.Close()

	// mesmo slug + mesmo conteúdo em projetos diferentes — o pior caso pro dedup.
	page := []compile.Page{{
		Slug:    "retrieval-tuning",
		Title:   "Retrieval Tuning",
		Content: "Ajuste de retrieval: rerank + threshold de similaridade calibrado por domínio.",
	}}

	// 1) projeto nexusyn → cria.
	n, err := persistPages(ctx, pool, nil, org, 0, "lesson", "nexusyn", page, []int64{1})
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	// 2) MESMO slug/conteúdo em reachyn → NÃO faz upsert no do nexusyn: cria separado.
	n, err = persistPages(ctx, pool, nil, org, 0, "lesson", "reachyn", page, []int64{2})
	require.NoError(t, err)
	assert.Equal(t, 1, n, "mesmo slug em outro projeto cria página separada (dedup escopado por project)")

	// 3) memória global (project "") → página global separada.
	n, err = persistPages(ctx, pool, nil, org, 0, "lesson", "", page, []int64{3})
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	assert.Equal(t, 3, countCurrentPages(ctx, t, adminURL, org),
		"3 páginas independentes: nexusyn + reachyn + global")

	// 4) re-compilar o MESMO projeto (nexusyn) → UPSERT, não duplica.
	n, err = persistPages(ctx, pool, nil, org, 0, "lesson", "nexusyn", page, []int64{1})
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, 3, countCurrentPages(ctx, t, adminURL, org),
		"re-compile do mesmo projeto faz upsert, mantém 3")

	// cada página carrega o project correto (global = NULL).
	assertProject(ctx, t, adminURL, org, "nexusyn")
	assertProject(ctx, t, adminURL, org, "reachyn")
	assertProject(ctx, t, adminURL, org, "")
}

// assertProject confere que existe exatamente 1 lesson viva com o slug no projeto
// dado e que a coluna project bate (NULL quando global).
func assertProject(ctx context.Context, t *testing.T, adminURL string, org int64, project string) {
	t.Helper()
	p, err := pgxpool.New(ctx, adminURL)
	require.NoError(t, err)
	defer p.Close()
	var got *string
	err = p.QueryRow(ctx,
		`SELECT project FROM pages
		 WHERE organization_id=$1 AND domain='lesson' AND slug='retrieval-tuning'
		   AND project IS NOT DISTINCT FROM NULLIF($2,'') AND valid_to IS NULL`,
		org, project).Scan(&got)
	require.NoError(t, err, "deveria haver 1 página no projeto %q", project)
	if project == "" {
		assert.Nil(t, got, "derivado de memória global deve ter project NULL")
	} else {
		require.NotNil(t, got, "derivado de projeto não pode ser global")
		assert.Equal(t, project, *got)
	}
}
