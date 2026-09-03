package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrTokenNotFound = hash não bate com nenhum token.
var ErrTokenNotFound = errors.New("auth: token não encontrado ou expirado")

// TokenRecord é o que retornamos após validação.
type TokenRecord struct {
	ID             int64
	OrganizationID int64
	Abilities      []string
	ExpiresAt      *time.Time
}

// HasAbility retorna true se "*" ou ability específica está em Abilities.
func (t *TokenRecord) HasAbility(ability string) bool {
	for _, a := range t.Abilities {
		if a == "*" || a == ability {
			return true
		}
	}
	return false
}

// FindToken consulta api_tokens pelo hash + verifica expires_at.
//
// IMPORTANTE — bypass de RLS:
//
// Quando o auth middleware roda, ainda não sabemos org_id (é justamente o que
// estamos tentando descobrir). Se a query rodar sob RLS normal (org_id NULL),
// a policy esconde TODOS os rows. Solução: usar a função SECURITY DEFINER
// `find_api_token_by_hash(hash)` definida na migration 0009, que roda como o
// owner (role nexus_service com BYPASSRLS).
//
// Como fallback (em ambiente onde a função não existe ainda), tentamos query direta
// — funciona se o usuário do pool tem BYPASSRLS (ex: dev local com superuser).
// Em prod, o user da app NÃO terá BYPASSRLS, então a função SECURITY DEFINER é
// obrigatória.
func FindToken(ctx context.Context, pool *pgxpool.Pool, hash string) (*TokenRecord, error) {
	// Tenta via função SECURITY DEFINER primeiro
	rec, err := findViaSecurityDefiner(ctx, pool, hash)
	if err == nil {
		return rec, nil
	}
	// Se função existe e simplesmente não achou (ErrTokenNotFound), propaga sem fallback
	// pra evitar latência extra. Outros erros (ex: função inexistente em deploy antigo)
	// caem no fallback direto.
	if errors.Is(err, ErrTokenNotFound) {
		return nil, err
	}

	// Fallback: query direta (funciona só se user tem BYPASSRLS — dev/CI)
	return findDirect(ctx, pool, hash)
}

func findViaSecurityDefiner(ctx context.Context, pool *pgxpool.Pool, hash string) (*TokenRecord, error) {
	const q = `SELECT id, organization_id, abilities, expires_at FROM find_api_token_by_hash($1)`

	var rec TokenRecord
	err := pool.QueryRow(ctx, q, hash).Scan(&rec.ID, &rec.OrganizationID, &rec.Abilities, &rec.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrTokenNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("auth: find token via function: %w", err)
	}
	return &rec, nil
}

func findDirect(ctx context.Context, pool *pgxpool.Pool, hash string) (*TokenRecord, error) {
	const q = `
		SELECT id, organization_id, abilities, expires_at
		FROM api_tokens
		WHERE token_hash = $1
		  AND (expires_at IS NULL OR expires_at > now())
	`
	var rec TokenRecord
	err := pool.QueryRow(ctx, q, hash).Scan(&rec.ID, &rec.OrganizationID, &rec.Abilities, &rec.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrTokenNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("auth: find token direct: %w", err)
	}
	return &rec, nil
}

// touchIntervalMin — throttle do TouchLastUsed: só grava last_used_at se o valor
// atual estiver mais velho que N minutos. Sem isso, o UPDATE disparava em
// goroutine nova em TODO request autenticado — contenção de lock na linha
// quente de api_tokens + bloat de dead-tuples (autovacuum) + churn de goroutine
// sob carga, pra um campo que só precisa de precisão de minutos (é telemetria
// de "último uso", não algo que exige exatidão por request). A condição vai
// dentro do próprio SQL (WHERE ... last_used_at < now() - interval) — idempotente
// e correta mesmo sob concorrência, sem precisar de estado em memória (cache/mutex).
// Configurável via NEXUS_TOKEN_TOUCH_INTERVAL_MIN; default 5min.
var touchIntervalMin = resolveTouchIntervalMin()

func resolveTouchIntervalMin() int {
	const def = 5
	v := os.Getenv("NEXUS_TOKEN_TOUCH_INTERVAL_MIN")
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// TouchLastUsed atualiza last_used_at do token (best-effort, não bloqueia request),
// mas só se o valor gravado estiver mais velho que touchIntervalMin (throttle —
// ver comentário acima). Roda async via goroutine.
//
// IMPORTANTE sobre context: criamos timeout fresh em vez de context.Background()
// puro pra evitar goroutine vazar se DB ficar lento. Não herdamos do ctx do request
// porque o request termina e cancelaria o UPDATE.
func TouchLastUsed(_ context.Context, pool *pgxpool.Pool, tokenID int64) {
	// Detach do ctx do request por design — UPDATE precisa rodar mesmo se request
	// terminou. ctx do request é deliberadamente ignorado.
	//nolint:contextcheck // detach intencional (goroutine sobrevive ao request)
	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = pool.Exec(bgCtx,
			`UPDATE api_tokens SET last_used_at = now()
			 WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < now() - ($2::int * interval '1 minute'))`,
			tokenID, touchIntervalMin)
	}()
}
