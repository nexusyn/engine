package tenant

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOrgIDFromContext_Missing(t *testing.T) {
	_, err := OrgIDFromContext(context.Background())
	assert.ErrorIs(t, err, ErrNoTenant)
}

func TestOrgIDFromContext_Set(t *testing.T) {
	ctx := WithOrgID(context.Background(), 42)
	got, err := OrgIDFromContext(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(42), got)
}

func TestOrgIDFromContext_RejectsZero(t *testing.T) {
	// Tenant ID 0 ou negativo é inválido — ErrNoTenant
	ctx := WithOrgID(context.Background(), 0)
	_, err := OrgIDFromContext(ctx)
	assert.ErrorIs(t, err, ErrNoTenant)

	ctx = WithOrgID(context.Background(), -5)
	_, err = OrgIDFromContext(ctx)
	assert.ErrorIs(t, err, ErrNoTenant)
}

func TestTokenIDFromContext(t *testing.T) {
	// Sem token id no ctx → 0
	assert.Equal(t, int64(0), TokenIDFromContext(context.Background()))

	ctx := WithTokenID(context.Background(), 99)
	assert.Equal(t, int64(99), TokenIDFromContext(ctx))
}

func TestContext_IsolatedFromOtherPackages(t *testing.T) {
	// O contextKey é unexported — garante que outros packages não conseguem
	// sobreescrever acidentalmente. Este teste é semântico — se compilar, passou.

	ctx := WithOrgID(context.Background(), 1)
	ctx = WithTokenID(ctx, 2)

	orgID, err := OrgIDFromContext(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), orgID)
	assert.Equal(t, int64(2), TokenIDFromContext(ctx))
}
