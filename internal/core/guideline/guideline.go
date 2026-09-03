// Package guideline carrega as guidelines vigentes de uma org de forma
// DETERMINÍSTICA (domain="guideline", não-deletadas) — sem embedding/busca.
//
// São os padrões obrigatórios que o agente deve ler ANTES de agir. Vivem como
// memória de domain="guideline" e são fixadas no topo de search_memory (MCP),
// devolvidas por get_guideline, e injetadas no pacote núcleo de GET /v1/context.
//
// Fonte única: tanto a camada MCP quanto a HTTP reusam Fetch — não duplicar a
// query.
package guideline

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nexusyn/engine/internal/tenant"
)

// Item é uma guideline (padrão obrigatório) da org.
type Item struct {
	ID      int64  `json:"id"`
	Title   string `json:"title"`
	Content string `json:"content"`
}

// Fetch carrega as guidelines vigentes da org (domain="guideline", valid_to IS
// NULL), ordenadas por id. Read-only, tenant-scoped. Retorna nil (sem erro)
// quando pool é nil — caller não precisa tratar pool ausente.
func Fetch(ctx context.Context, pool *pgxpool.Pool, orgID int64) ([]Item, error) {
	if pool == nil {
		return nil, nil
	}
	var items []Item
	err := tenant.RunWithTenantReadOnly(ctx, pool, orgID, func(tx pgx.Tx) error {
		rows, e := tx.Query(ctx,
			`SELECT id, title, content FROM pages
			 WHERE organization_id = $1 AND domain = 'guideline' AND valid_to IS NULL
			 ORDER BY id`, orgID)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var it Item
			if e := rows.Scan(&it.ID, &it.Title, &it.Content); e != nil {
				return e
			}
			items = append(items, it)
		}
		return rows.Err()
	})
	return items, err
}
