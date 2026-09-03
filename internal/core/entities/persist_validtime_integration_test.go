//go:build integration

// persist_validtime_integration_test.go — Módulo C: edge com valid_at/invalid_at
// extraídos (ISO 8601) vira valid_from/valid_to REAIS no banco (não só now()).
// Reusa setupPG/seedOrgAndPage do persist_integration_test.go (mesmo package).
package entities_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexusyn/engine/internal/core/entities"
)

func TestPersist_ValidTime(t *testing.T) {
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

	extracted := &entities.Extracted{
		Entities: []entities.EntityRef{
			{Name: "Luna", Kind: "person"},
			{Name: "Lavras", Kind: "place"},
		},
		Edges: []entities.EdgeRef{
			// fato com validade explícita: morou em Lavras de 2018 até 2020-06.
			{FromName: "Luna", ToName: "Lavras", Kind: "located_in", Weight: 1.0, ValidAt: "2018-01-01", InvalidAt: "2020-06-15"},
		},
	}
	_, err = entities.RunInTenantTx(ctx, pool, orgID, pageID, extracted)
	require.NoError(t, err)

	adminPool, err := pgxpool.New(ctx, adminURL)
	require.NoError(t, err)
	defer adminPool.Close()
	var vf time.Time
	var vt *time.Time
	require.NoError(t, adminPool.QueryRow(ctx,
		`SELECT valid_from, valid_to FROM edges WHERE organization_id=$1 AND kind='located_in'`, orgID).Scan(&vf, &vt))
	assert.Equal(t, 2018, vf.UTC().Year(), "valid_from = valid_at extraido")
	require.NotNil(t, vt, "valid_to setado = invalid_at extraido")
	assert.Equal(t, 2020, vt.UTC().Year())
	assert.Equal(t, time.June, vt.UTC().Month())
}
