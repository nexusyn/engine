//go:build integration

// context_integration_test.go — smoke ponta-a-ponta do GET /v1/context contra
// Postgres real (testcontainers + migrations reais). Reusa setupPostgres do
// admin_delete_integration_test.go (mesmo pacote api_test).
//
// Rodar: go test -tags=integration -run TestContext ./internal/api/...
package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexusyn/engine/internal/api"
	"github.com/nexusyn/engine/internal/tenant"
)

func TestContextHandler_EndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}
	ctx := context.Background()
	pool, cleanup := setupPostgres(ctx, t)
	defer cleanup()

	const org = int64(2)

	// Seed: org + agent + guideline + wiki (project nexusyn) + profile.
	_, err := pool.Exec(ctx, `INSERT INTO organizations (id, slug, name) VALUES ($1, 'example.com', 'RedFoxCode')`, org)
	require.NoError(t, err)
	var agentID int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO agents (organization_id, slug, name) VALUES ($1, 'claude', 'claude') RETURNING id`, org).Scan(&agentID))

	_, err = pool.Exec(ctx,
		`INSERT INTO pages (organization_id, agent_id, slug, title, content, domain)
		 VALUES ($1, $2, 'g1', 'Regra de ouro', 'Verificar o estado vivo antes de afirmar.', 'guideline')`, org, agentID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx,
		`INSERT INTO pages (organization_id, agent_id, slug, title, content, domain, project)
		 VALUES ($1, $2, 'w1', 'Memória por projeto', 'No ar em produção; recall = projeto + globais.', 'wiki', 'nexusyn')`, org, agentID)
	require.NoError(t, err)
	// Página wiki de OUTRO projeto — não pode aparecer no índice de nexusyn (filtro estrito).
	_, err = pool.Exec(ctx,
		`INSERT INTO pages (organization_id, agent_id, slug, title, content, domain, project)
		 VALUES ($1, $2, 'w2', 'Reachyn billing', 'Self-service Zernio.', 'wiki', 'reachyn')`, org, agentID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx,
		`INSERT INTO user_profiles (organization_id, content, preferences_count, lessons_count)
		 VALUES ($1, '## Likes\n- PT-BR, direto ao ponto', 1, 0)`, org)
	require.NoError(t, err)
	// Memória recente de nexusyn (entra em "últimas mudanças"; domain memory).
	_, err = pool.Exec(ctx,
		`INSERT INTO pages (organization_id, agent_id, slug, title, content, domain, project)
		 VALUES ($1, $2, 'm1', 'Deploy do endpoint context', 'Subiu em prod.', 'memory', 'nexusyn')`, org, agentID)
	require.NoError(t, err)

	// --- Happy path (JSON) ---
	req := httptest.NewRequest(http.MethodGet, "/v1/context?project=nexusyn&max_tokens=600", nil)
	req = req.WithContext(tenant.WithOrgID(req.Context(), org))
	rec := httptest.NewRecorder()
	api.ContextHandler(pool)(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var resp api.ContextResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	assert.Equal(t, org, resp.OrganizationID)
	assert.Equal(t, "nexusyn", resp.Project)
	require.Len(t, resp.Guidelines, 1, "deve trazer a guideline seedada")
	assert.Equal(t, "Regra de ouro", resp.Guidelines[0].Title)
	assert.Contains(t, resp.Profile.Content, "PT-BR")
	assert.Equal(t, 1, resp.Profile.PreferencesCount)

	// Índice do projeto: só a wiki de nexusyn (filtro estrito — reachyn fora).
	require.Len(t, resp.ProjectPages, 1, "filtro estrito de project: só wiki de nexusyn")
	assert.Equal(t, "Memória por projeto", resp.ProjectPages[0].Title)

	// Últimas mudanças: memória + wiki de nexusyn (exclui guideline e reachyn).
	require.GreaterOrEqual(t, len(resp.RecentChanges), 1, "deve trazer mudanças recentes")
	var recentTitles []string
	for _, rc := range resp.RecentChanges {
		recentTitles = append(recentTitles, rc.Title)
		assert.NotEqual(t, "guideline", rc.Domain, "guideline não é 'mudança'")
	}
	assert.Contains(t, recentTitles, "Deploy do endpoint context")
	assert.NotContains(t, recentTitles, "Reachyn billing", "outro projeto não pode vazar nas mudanças")

	// Pacote renderizado contém as 4 seções e o cabeçalho.
	for _, want := range []string{"org 2", "projeto nexusyn", "Guidelines da org", "Regra de ouro", "Perfil do usuário", "Índice do projeto: nexusyn", "Memória por projeto", "Últimas mudanças", "Deploy do endpoint context"} {
		assert.Contains(t, resp.Context, want)
	}
	assert.NotContains(t, resp.Context, "Reachyn billing", "wiki de outro projeto não pode vazar")
	assert.Equal(t, 600, resp.Meta.MaxTokens)
	assert.Greater(t, resp.Meta.ApproxTokens, 0)
	// Calibração: o pacote fica dentro do teto de tokens (margem p/ header +
	// marcadores de truncagem). approx_tokens = runas/ctxCharsPerToken.
	assert.LessOrEqual(t, resp.Meta.ApproxTokens, resp.Meta.MaxTokens+50,
		"pacote estourou o teto de tokens (budget não segurou)")

	// --- format=markdown devolve só o pacote, text/markdown ---
	req2 := httptest.NewRequest(http.MethodGet, "/v1/context?project=nexusyn&format=markdown", nil)
	req2 = req2.WithContext(tenant.WithOrgID(req2.Context(), org))
	rec2 := httptest.NewRecorder()
	api.ContextHandler(pool)(rec2, req2)
	require.Equal(t, http.StatusOK, rec2.Code)
	assert.True(t, strings.HasPrefix(rec2.Header().Get("Content-Type"), "text/markdown"))
	assert.True(t, strings.HasPrefix(rec2.Body.String(), "# Nexusyn — Pacote de Contexto"))

	// --- sections: só guidelines (perfil/índice/recentes fora) ---
	reqS := httptest.NewRequest(http.MethodGet, "/v1/context?project=nexusyn&sections=guidelines", nil)
	reqS = reqS.WithContext(tenant.WithOrgID(reqS.Context(), org))
	recS := httptest.NewRecorder()
	api.ContextHandler(pool)(recS, reqS)
	require.Equal(t, http.StatusOK, recS.Code)
	var respS api.ContextResponse
	require.NoError(t, json.Unmarshal(recS.Body.Bytes(), &respS))
	require.Len(t, respS.Guidelines, 1, "guidelines deve vir")
	assert.Empty(t, respS.ProjectPages, "índice excluído por sections")
	assert.Empty(t, respS.RecentChanges, "recentes excluídos por sections")
	assert.Equal(t, []string{"guidelines"}, respS.Meta.Sections)
	assert.Contains(t, respS.Context, "Guidelines da org")
	assert.NotContains(t, respS.Context, "Índice do projeto")
	assert.NotContains(t, respS.Context, "Últimas mudanças")

	// --- org suspensa → 403 (AUD-010) ---
	_, err = pool.Exec(ctx, `UPDATE organizations SET suspended_at = now() WHERE id = $1`, org)
	require.NoError(t, err)
	req3 := httptest.NewRequest(http.MethodGet, "/v1/context", nil)
	req3 = req3.WithContext(tenant.WithOrgID(req3.Context(), org))
	rec3 := httptest.NewRecorder()
	api.ContextHandler(pool)(rec3, req3)
	assert.Equal(t, http.StatusForbidden, rec3.Code)
}

func TestContextHandler_NoTenant_401(t *testing.T) {
	// Sem org no contexto → 401 (não precisa de DB; pool nil nunca é tocado).
	req := httptest.NewRequest(http.MethodGet, "/v1/context", nil)
	rec := httptest.NewRecorder()
	api.ContextHandler(nil)(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}
