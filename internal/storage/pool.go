// Package storage abstrai acesso ao Postgres via pgx.
//
// Responsabilidades:
//   - Inicializar pgxpool (com config tuning)
//   - Registrar tipos custom (vector via pgvector-go)
//   - Health check do DB
//
// Não contém regras de negócio — outras camadas (core/, job/) usam *pgxpool.Pool.
package storage

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgxvector "github.com/pgvector/pgvector-go/pgx"

	"github.com/nexusyn/engine/internal/config"
)

// NewPool cria um pgxpool conforme cfg.DB.
// Registra tipos custom (vector) automaticamente em cada conexão nova.
func NewPool(ctx context.Context, cfg config.DBConfig) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parse db url: %w", err)
	}

	poolCfg.MaxConns = cfg.MaxConns
	poolCfg.MinConns = cfg.MinConns

	// AfterConnect: registra tipos custom em cada nova conexão do pool.
	// Sem isso, queries com vector(1024) falham com "unsupported type" no scan.
	poolCfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		return registerCustomTypes(ctx, conn)
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("new pool: %w", err)
	}

	// Ping pra falhar cedo se DB inacessível
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping db: %w", err)
	}

	return pool, nil
}

// registerCustomTypes é chamado em cada conexão nova.
// Registra o tipo `vector` do pgvector pra que pgx faça scan/encode automático
// de slices []float32 ↔ vector(N).
//
// Se a extensão não estiver instalada (CREATE EXTENSION vector não rodou),
// retorna nil (não-fatal — queries com vector ainda falhariam, mas conexões
// não-vetoriais funcionam).
func registerCustomTypes(ctx context.Context, conn *pgx.Conn) error {
	if err := pgxvector.RegisterTypes(ctx, conn); err != nil {
		// Não-fatal: log e segue. Em DB sem extensão vector, queries vetoriais
		// vão falhar mas o pool funciona pra outras queries (health, auth).
		return nil //nolint:nilerr // intencional: registration opcional
	}
	return nil
}

// Health verifica se o pool consegue executar uma query simples.
// Usado pelo /ready endpoint (não /health — health é liveness do processo).
func Health(ctx context.Context, pool *pgxpool.Pool) error {
	var n int
	if err := pool.QueryRow(ctx, "SELECT 1").Scan(&n); err != nil {
		return fmt.Errorf("db health: %w", err)
	}
	return nil
}
