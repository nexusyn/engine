package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nexusyn/engine/internal/config"
	"github.com/nexusyn/engine/internal/dateutil"
)

// runReindexDates faz o BACKFILL do canal de datas (migration 0022): varre os
// chunks existentes, extrai as datas canônicas do content e popula chunks.dates.
// Idempotente — chunks sem data ficam com '' (default) e são pulados em re-runs.
//
// Usa ADMIN_DATABASE_URL (role nexus = superuser do container, bypassa RLS) pra
// ver/atualizar chunks de todas as orgs num passe só. One-off pós-deploy.
func runReindexDates(args []string) error {
	ctx := context.Background()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config load: %w", err)
	}
	pool, err := pgxpool.New(ctx, cfg.DB.AdminOrDefault())
	if err != nil {
		return fmt.Errorf("db pool: %w", err)
	}
	defer pool.Close()

	rows, err := pool.Query(ctx, `SELECT id, content FROM chunks WHERE dates = ''`)
	if err != nil {
		return fmt.Errorf("select chunks: %w", err)
	}
	type chunk struct {
		id      int64
		content string
	}
	var todo []chunk
	for rows.Next() {
		var c chunk
		if err := rows.Scan(&c.id, &c.content); err != nil {
			rows.Close()
			return fmt.Errorf("scan: %w", err)
		}
		todo = append(todo, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("rows: %w", err)
	}

	updated := 0
	for _, c := range todo {
		toks := dateutil.DateSearchTokens(c.content)
		if toks == "" {
			continue // sem data no chunk — deixa '' (default)
		}
		if _, err := pool.Exec(ctx, `UPDATE chunks SET dates = $1 WHERE id = $2`, toks, c.id); err != nil {
			return fmt.Errorf("update chunk %d: %w", c.id, err)
		}
		updated++
	}
	slog.Info("reindex-dates done", "scanned", len(todo), "updated", updated)
	fmt.Printf("reindex-dates: %d chunks varridos, %d com data populados\n", len(todo), updated)
	return nil
}
