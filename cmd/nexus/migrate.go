package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // pgx em modo database/sql (pra goose)
	"github.com/pressly/goose/v3"

	"github.com/nexusyn/engine/internal/config"
	"github.com/nexusyn/engine/internal/job"
	"github.com/nexusyn/engine/migrations"
)

// runMigrate é o entrypoint do subcommand `nexus migrate <up|down|status|reset>`.
func runMigrate(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("uso: nexus migrate <up|down|status|reset|version>")
	}
	subcmd := args[0]

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	// Migrations rodam com ADMIN_DATABASE_URL (privilegiada) — fallback pra DATABASE_URL
	dbURL := cfg.DB.AdminOrDefault()

	// pgx em modo database/sql — goose precisa de *sql.DB
	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.PingContext(context.Background()); err != nil {
		return fmt.Errorf("ping db: %w", err)
	}

	// Configurar goose pra usar nossa FS embutida
	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set dialect: %w", err)
	}

	// O path "." é o root do embed.FS (que já contém os .sql diretamente)
	dir := "."

	slog.Info("migrate", "subcmd", subcmd, "db", maskURL(dbURL))

	switch subcmd {
	case "up":
		if err := goose.Up(db, dir); err != nil {
			return fmt.Errorf("goose up: %w", err)
		}
		// Após nossas migrations, rodar as do River.
		// River tem migration table própria (river_migration) — independente do
		// goose_db_version. Idempotente.
		if err := runRiverMigrate(dbURL); err != nil {
			return err
		}
		// Turnkey self-host: cria o token master a partir de NEXUS_BOOTSTRAP_SECRET
		// num DB fresco (idempotente). Não-fatal se falhar.
		if err := SeedBootstrapToken(db); err != nil {
			slog.Warn("seed bootstrap token falhou (não-fatal)", "err", err)
		}
		return nil
	case "down":
		return goose.Down(db, dir)
	case "status":
		return goose.Status(db, dir)
	case "version":
		return goose.Version(db, dir)
	case "reset":
		return goose.Reset(db, dir)
	default:
		return fmt.Errorf("subcommand desconhecido: %s (use up|down|status|reset|version)", subcmd)
	}
}

// runRiverMigrate aplica as migrations do River no DB.
// Idempotente — pode ser chamado multiplas vezes.
func runRiverMigrate(dbURL string) error {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("river migrate pool: %w", err)
	}
	defer pool.Close()
	if err := job.MigrateUp(ctx, pool); err != nil {
		return err
	}
	return grantRiverMaintain(ctx, pool)
}

// grantRiverMaintain concede MAINTAIN nas river_* ao role nexus_app.
// Why: River tables são criadas pelo admin (nexus, superuser/owner) mas o
// worker conecta como nexus_app via DATABASE_URL. Sem MAINTAIN, o Reindexer
// do River loga 'permission denied for index river_job_*' a cada 24h.
// PG17 introduziu MAINTAIN que permite REINDEX sem ownership.
func grantRiverMaintain(ctx context.Context, pool *pgxpool.Pool) error {
	const stmt = `
DO $$
DECLARE t record;
BEGIN
  FOR t IN SELECT tablename FROM pg_tables
           WHERE tablename LIKE 'river_%' AND schemaname='public'
  LOOP
    EXECUTE format('GRANT MAINTAIN ON TABLE %I TO nexus_app', t.tablename);
  END LOOP;
END$$;`
	if _, err := pool.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("grant maintain on river_*: %w", err)
	}
	return nil
}

// maskURL esconde a senha de URLs Postgres pra logging seguro.
// "postgres://user:secret@host/db" → "postgres://user:***@host/db"
func maskURL(url string) string {
	// Implementação simples — não usa net/url pra evitar deps em hot path
	idxAt := -1
	idxColon := -1
	for i := 0; i < len(url); i++ {
		if url[i] == '@' {
			idxAt = i
			break
		}
	}
	if idxAt == -1 {
		return url
	}
	for i := 0; i < idxAt; i++ {
		if url[i] == ':' && i > 10 { // pula "postgres://"
			idxColon = i
			break
		}
	}
	if idxColon == -1 {
		return url
	}
	return url[:idxColon+1] + "***" + url[idxAt:]
}

func init() {
	// Silencia log default do goose (usa nosso slog)
	goose.SetLogger(goose.NopLogger())
}

// Garante que os imports não sejam removidos pelo linter
var _ = os.Args
