//go:build integration

// import_integration_test.go — gate do POST /v1/import (round-trip do export):
// enfileira 1 IngestJob por page válida preservando created_at/project/agent,
// pula derivados e content vazio, e rejeita lote acima do cap. Também cobre o
// export fiel (GET /v1/export com project+agent). Reusa setupPostgres do
// admin_delete_integration_test.go (mesmo pacote api_test).
//
// Rodar: go test -tags=integration -run TestImport ./internal/api/...
package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexusyn/engine/internal/api"
	"github.com/nexusyn/engine/internal/job"
	"github.com/nexusyn/engine/internal/tenant"
)

func TestImportHandler_EndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}
	ctx := context.Background()
	pool, cleanup := setupPostgres(ctx, t)
	defer cleanup()

	// Tabelas do River (o handler só enfileira; quem processa é o worker).
	require.NoError(t, job.MigrateUp(ctx, pool))

	const org = int64(2)
	_, err := pool.Exec(ctx, `INSERT INTO organizations (id, slug, name) VALUES ($1, 'example.com', 'RedFoxCode')`, org)
	require.NoError(t, err)

	original := time.Date(2025, 3, 14, 9, 26, 53, 0, time.UTC)
	body := map[string]any{
		"pages": []map[string]any{
			{"title": "Válida com tudo", "content": "conteúdo 1", "domain": "memory",
				"project": "nexusyn", "agent": "claude", "created_at": original},
			{"title": "Knowledge sem agent", "content": "conteúdo 2", "domain": "knowledge"},
			{"title": "Derivada", "content": "wiki compilada", "domain": "wiki"},   // pulada
			{"title": "Vazia", "content": "   ", "domain": "memory"},               // pulada
			{"title": "Domain desconhecido", "content": "x", "domain": "banana"},   // pulada
		},
	}
	raw, err := json.Marshal(body)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/v1/import", bytes.NewReader(raw))
	req = req.WithContext(tenant.WithOrgID(req.Context(), org))
	rec := httptest.NewRecorder()
	api.ImportHandler(pool)(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code, "body=%s", rec.Body.String())
	var resp api.ImportResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, 2, resp.Queued)
	assert.Equal(t, 3, resp.Skipped)
	assert.Equal(t, 2, resp.SkippedReasons["derived_domain"], "wiki + domain desconhecido")
	assert.Equal(t, 1, resp.SkippedReasons["empty_content"])
	assert.Len(t, resp.JobIDs, 2)

	// Jobs no River com os args certos (created_at/project/agent_id preservados).
	var kinds int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM river_job WHERE kind = 'ingest'`).Scan(&kinds))
	assert.Equal(t, 2, kinds)

	var argsJSON []byte
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT args FROM river_job WHERE kind = 'ingest' AND args->>'title' = 'Válida com tudo'`).Scan(&argsJSON))
	var got job.IngestArgs
	require.NoError(t, json.Unmarshal(argsJSON, &got))
	assert.Equal(t, org, got.OrganizationID)
	assert.Equal(t, "nexusyn", got.Project)
	assert.NotZero(t, got.AgentID, "agent claude resolvido (find-or-create)")
	require.NotNil(t, got.CreatedAt)
	assert.True(t, got.CreatedAt.Equal(original), "created_at preservado no job")

	// Agent find-or-create aconteceu de fato.
	var agents int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM agents WHERE organization_id=$1 AND slug='claude'`, org).Scan(&agents))
	assert.Equal(t, 1, agents)

	// Payload vazio → 400.
	req = httptest.NewRequest(http.MethodPost, "/v1/import", bytes.NewReader([]byte(`{"pages":[]}`)))
	req = req.WithContext(tenant.WithOrgID(req.Context(), org))
	rec = httptest.NewRecorder()
	api.ImportHandler(pool)(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	// Acima do cap de 1000 → 400.
	big := make([]map[string]any, 1001)
	for i := range big {
		big[i] = map[string]any{"title": fmt.Sprintf("p%d", i), "content": "x"}
	}
	raw, err = json.Marshal(map[string]any{"pages": big})
	require.NoError(t, err)
	req = httptest.NewRequest(http.MethodPost, "/v1/import", bytes.NewReader(raw))
	req = req.WithContext(tenant.WithOrgID(req.Context(), org))
	rec = httptest.NewRecorder()
	api.ImportHandler(pool)(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestImportExport_RoundTripFields(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}
	ctx := context.Background()
	pool, cleanup := setupPostgres(ctx, t)
	defer cleanup()

	const org = int64(3)
	_, err := pool.Exec(ctx, `INSERT INTO organizations (id, slug, name) VALUES ($1, 'org-x', 'Org X')`, org)
	require.NoError(t, err)
	var agentID int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO agents (organization_id, slug, name) VALUES ($1, 'claude', 'claude') RETURNING id`, org).Scan(&agentID))
	_, err = pool.Exec(ctx,
		`INSERT INTO pages (organization_id, agent_id, slug, title, content, domain, project)
		 VALUES ($1, $2, 'm1', 'Com projeto', 'conteúdo', 'memory', 'nexusyn'),
		        ($1, NULL, 'm2', 'Global sem agent', 'conteúdo 2', 'memory', NULL)`, org, agentID)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/v1/export?domain=memory", nil)
	req = req.WithContext(tenant.WithOrgID(req.Context(), org))
	rec := httptest.NewRecorder()
	api.ExportHandler(pool)(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var resp struct {
		Pages []struct {
			Title   string `json:"title"`
			Project string `json:"project"`
			Agent   string `json:"agent"`
		} `json:"pages"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Pages, 2)
	byTitle := map[string][2]string{}
	for _, p := range resp.Pages {
		byTitle[p.Title] = [2]string{p.Project, p.Agent}
	}
	assert.Equal(t, [2]string{"nexusyn", "claude"}, byTitle["Com projeto"], "export fiel: project+agent")
	assert.Equal(t, [2]string{"", ""}, byTitle["Global sem agent"])
}
