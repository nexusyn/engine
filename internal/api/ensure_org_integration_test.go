//go:build integration

// ensure_org_integration_test.go — gate da idempotência do `nexus admin create-token`
// (função ensure_org_with_token, migração 0029). Reusa setupPostgres do
// admin_delete_integration_test.go (mesmo pacote api_test).
package api_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnsureOrgWithToken_Idempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}
	ctx := context.Background()
	pool, cleanup := setupPostgres(ctx, t)
	defer cleanup()

	const q = `SELECT org_id, token_id, org_created FROM ensure_org_with_token('default','Default',$1,'{*}'::text[])`

	// 1ª chamada num DB fresco: cria a org.
	var org1, tok1 int64
	var created1 bool
	require.NoError(t, pool.QueryRow(ctx, q, "hash-1").Scan(&org1, &tok1, &created1))
	assert.True(t, created1, "1ª chamada deve criar a org")

	// 2ª chamada com o MESMO slug: NÃO falha, reusa a org, emite token novo.
	var org2, tok2 int64
	var created2 bool
	require.NoError(t, pool.QueryRow(ctx, q, "hash-2").Scan(&org2, &tok2, &created2))
	assert.False(t, created2, "2ª chamada deve reusar a org (idempotente)")
	assert.Equal(t, org1, org2, "mesmo org_id")
	assert.NotEqual(t, tok1, tok2, "token novo a cada chamada")

	// Só uma org com esse slug (não duplicou).
	var orgs int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM organizations WHERE slug = 'default'`).Scan(&orgs))
	assert.Equal(t, 1, orgs)
	// Os dois tokens coexistem (o antigo continua válido).
	var toks int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM api_tokens WHERE organization_id = $1`, org1).Scan(&toks))
	assert.Equal(t, 2, toks)
}
