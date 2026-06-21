// Package tenant gerencia o binding de tenant (organização) por request.
//
// Filosofia:
//   - org_id flui via context.Context, NUNCA via globals
//   - HTTP middleware extrai org_id do token autenticado e seta no ctx
//   - Workers (River) extraem org_id dos args do job e bindam via RunWithTenant
//   - Toda query que toca tabela com RLS DEVE rodar dentro de RunWithTenant
package tenant

import (
	"context"
	"errors"
)

// contextKey é o tipo privado pra evitar colisão com outros packages.
// (idiom Go: usar tipo unexported como chave de ctx)
type contextKey int

const (
	keyOrgID contextKey = iota
	keyTokenID
	keyAbilities
)

// ErrNoTenant é retornado quando uma operação RLS-sensitive é chamada sem org_id no ctx.
var ErrNoTenant = errors.New("tenant: org_id ausente no context (esqueceu RunWithTenant ou auth middleware?)")

// WithOrgID retorna ctx contendo o org_id.
// Usado pelo auth middleware após validar token.
func WithOrgID(ctx context.Context, orgID int64) context.Context {
	return context.WithValue(ctx, keyOrgID, orgID)
}

// OrgIDFromContext extrai org_id do ctx. Retorna ErrNoTenant se ausente.
func OrgIDFromContext(ctx context.Context) (int64, error) {
	v := ctx.Value(keyOrgID)
	if v == nil {
		return 0, ErrNoTenant
	}
	id, ok := v.(int64)
	if !ok || id <= 0 {
		return 0, ErrNoTenant
	}
	return id, nil
}

// WithTokenID anexa o ID do api_token autenticado (útil pra audit/rate-limit).
func WithTokenID(ctx context.Context, tokenID int64) context.Context {
	return context.WithValue(ctx, keyTokenID, tokenID)
}

// TokenIDFromContext extrai o ID do api_token, ou 0 se ausente (rotas públicas).
func TokenIDFromContext(ctx context.Context) int64 {
	v := ctx.Value(keyTokenID)
	if v == nil {
		return 0
	}
	id, ok := v.(int64)
	if !ok {
		return 0
	}
	return id
}

// WithAbilities anexa as abilities (scopes) do token autenticado ao ctx.
func WithAbilities(ctx context.Context, abilities []string) context.Context {
	return context.WithValue(ctx, keyAbilities, abilities)
}

// AbilitiesFromContext extrai as abilities do token, ou nil se ausente.
func AbilitiesFromContext(ctx context.Context) []string {
	v := ctx.Value(keyAbilities)
	if v == nil {
		return nil
	}
	a, ok := v.([]string)
	if !ok {
		return nil
	}
	return a
}

// HasAbility retorna true se as abilities contêm "*" (curinga) ou a ability exata.
func HasAbility(abilities []string, want string) bool {
	for _, a := range abilities {
		if a == "*" || a == want {
			return true
		}
	}
	return false
}
