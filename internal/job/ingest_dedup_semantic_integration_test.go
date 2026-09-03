//go:build integration

// ingest_dedup_semantic_integration_test.go — Módulo A: gate do dedup semântico.
// Reusa setupPG/seedOrg/countCurrentPages/ingestJob do ingest_dedup_integration_test.go
// (mesmo package). Usa um mock embed pra controlar a similaridade.
package job

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexusyn/engine/internal/provider/embed"
)

// mockEmbed retorna vetores 1024-dim determinísticos por palavra-chave: textos que
// compartilham a keyword recebem o MESMO vetor (cosine 1.0); keywords diferentes →
// vetores ortogonais (cosine 0). Suficiente pra exercitar o gate do dedup semântico.
type mockEmbed struct{}

func (mockEmbed) Embed(_ context.Context, texts []string, _ embed.InputType) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, 1024)
		low := strings.ToLower(t)
		switch {
		case strings.Contains(low, "pizza"):
			v[0] = 1
		case strings.Contains(low, "reachyn"):
			v[1] = 1
		default:
			v[2] = 1
		}
		out[i] = v
	}
	return out, nil
}

func (mockEmbed) Name() string  { return "mock" }
func (mockEmbed) Model() string { return "mock" }
func (mockEmbed) Dim() int      { return 1024 }

// mockResolver satisfaz embedResolver (resolve o provider ao vivo).
type mockResolver struct{ p embed.Provider }

func (m mockResolver) Embed(context.Context) embed.Provider { return m.p }

func TestIngest_DedupSemantico(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	ctx := context.Background()
	appURL, adminURL, cleanup := setupPG(ctx, t)
	defer cleanup()
	org := seedOrg(ctx, t, adminURL)

	pool, err := pgxpool.New(ctx, appURL)
	require.NoError(t, err)
	w := NewIngestWorker(pool, mockResolver{mockEmbed{}})

	// page A (keyword "pizza")
	require.NoError(t, w.Work(ctx, ingestJob(1, org, "A", "Luciano gosta de pizza de calabresa", "memory")))
	assert.Equal(t, 1, countCurrentPages(ctx, t, adminURL, org))

	// paráfrase: conteúdo diferente (hash diferente, escapa do determinístico) mas
	// mesma keyword → embedding idêntico (cosine 1.0) → supersede a anterior.
	require.NoError(t, w.Work(ctx, ingestJob(2, org, "B", "O Luciano adora muito pizza", "memory")))
	assert.Equal(t, 1, countCurrentPages(ctx, t, adminURL, org), "parafrase supersede a anterior")

	// conteúdo distinto (keyword "reachyn", embedding ortogonal) → ADD
	require.NoError(t, w.Work(ctx, ingestJob(3, org, "C", "Deploy do Reachyn na VPS hostinger", "memory")))
	assert.Equal(t, 2, countCurrentPages(ctx, t, adminURL, org), "conteudo distinto cria page")
}
