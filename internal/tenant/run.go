package tenant

import (
	"context"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RunWithTenant executa fn dentro de uma transação Postgres com o tenant bindado.
//
// Mecânica:
//  1. Abre transação
//  2. Executa `SET LOCAL nexus.org_id = $tenantID` — descartado no COMMIT/ROLLBACK
//  3. Chama fn(tx) — todas queries enxergam apenas rows do tenant via RLS
//  4. Commit se sem erro; Rollback se erro
//
// IMPORTANTE: NUNCA use `SET nexus.org_id` (sem LOCAL) — o valor persistiria na
// conexão e vazaria pro próximo request quando a conexão voltasse pro pool.
// É a definição do "RLS leak" — bug crítico do mnemonexus que evitamos aqui.
//
// Uso típico (handler HTTP):
//
//	orgID, err := tenant.OrgIDFromContext(r.Context())
//	if err != nil { ... 401 ... }
//	err = tenant.RunWithTenant(r.Context(), pool, orgID, func(tx pgx.Tx) error {
//	    _, err := tx.Exec(ctx, "INSERT INTO pages ...")
//	    return err
//	})
//
// Uso típico (River worker):
//
//	func (w *IngestWorker) Work(ctx context.Context, job *river.Job[Args]) error {
//	    return tenant.RunWithTenant(ctx, w.pool, job.Args.OrgID, func(tx pgx.Tx) error {
//	        return doIngest(ctx, tx, job.Args.PageID)
//	    })
//	}
func RunWithTenant(ctx context.Context, pool *pgxpool.Pool, tenantID int64, fn func(pgx.Tx) error) error {
	if tenantID <= 0 {
		return fmt.Errorf("tenant: invalid tenantID %d", tenantID)
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("tenant: begin tx: %w", err)
	}
	defer func() {
		// Rollback é no-op se já houve commit
		_ = tx.Rollback(ctx)
	}()

	// set_config(name, value, is_local=true) — equivalente a SET LOCAL mas aceita
	// parâmetros via $N. SET LOCAL puro NÃO aceita placeholder ($1 dá syntax error).
	// is_local=true → descartado no COMMIT/ROLLBACK, não vaza no pool.
	if _, err := tx.Exec(ctx, "SELECT set_config('nexus.org_id', $1, true)", strconv.FormatInt(tenantID, 10)); err != nil {
		return fmt.Errorf("tenant: bind org_id: %w", err)
	}

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("tenant: commit: %w", err)
	}
	return nil
}

// RunWithTenantReadOnly é como RunWithTenant mas marca a tx como read-only.
// Pequena otimização do planner pra queries de leitura puras.
func RunWithTenantReadOnly(ctx context.Context, pool *pgxpool.Pool, tenantID int64, fn func(pgx.Tx) error) error {
	if tenantID <= 0 {
		return fmt.Errorf("tenant: invalid tenantID %d", tenantID)
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("tenant: begin ro tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('nexus.org_id', $1, true)", strconv.FormatInt(tenantID, 10)); err != nil {
		return fmt.Errorf("tenant: bind org_id: %w", err)
	}

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("tenant: commit ro: %w", err)
	}
	return nil
}
