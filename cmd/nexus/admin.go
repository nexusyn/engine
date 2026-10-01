package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/nexusyn/engine/internal/config"
)

// SeedBootstrapToken cria uma org+token "master" (ability *) a partir de
// NEXUS_BOOTSTRAP_SECRET — mas SÓ num DB fresco (sem nenhum token). Torna o
// self-host turnkey: o token mestre fica determinístico = 1|<secret>
// (o console usa NEXUS_MASTER_TOKEN="1|${NEXUS_BOOTSTRAP_SECRET}"). Idempotente:
// se já houver token, não faz nada. Chamado pelo `migrate up`.
func SeedBootstrapToken(db *sql.DB) error {
	secret := os.Getenv("NEXUS_BOOTSTRAP_SECRET")
	if secret == "" {
		return nil // feature desligada
	}
	var n int
	if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM api_tokens`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil // já tem token(s) — não mexe
	}
	sum := sha256.Sum256([]byte(secret))
	hash := hex.EncodeToString(sum[:])
	var orgID, tokenID int64
	err := db.QueryRowContext(context.Background(),
		`SELECT org_id, token_id FROM create_org_with_token('master', 'Master', $1, '{*}'::text[])`,
		hash).Scan(&orgID, &tokenID)
	if err != nil {
		return err
	}
	if tokenID != 1 {
		slog.Warn("bootstrap token id != 1 — o console deve usar este id no NEXUS_MASTER_TOKEN", "token_id", tokenID)
	}
	slog.Info("bootstrap token seeded (turnkey)", "org_id", orgID, "token_id", tokenID)
	return nil
}

// runAdmin — control-plane do operador (self-host / bootstrap).
//
//	nexus admin create-token <slug> <name> [abilities]
//	   cria uma org + token e imprime o token. abilities default = "*" (admin).
//	   Use o token impresso como master token do console (NEXUS_MASTER_TOKEN) ou
//	   pra chamar /v1/admin/*. Roda contra ADMIN_DATABASE_URL (fallback DATABASE_URL).
func runAdmin(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("uso: nexus admin create-token <slug> <name> [abilities]")
	}
	switch args[0] {
	case "create-token":
		return adminCreateToken(args[1:])
	case "set-flag":
		return adminSetFlag(args[1:])
	default:
		return fmt.Errorf("admin: subcomando desconhecido %q (disponível: create-token, set-flag)", args[0])
	}
}

// adminFlags lista as feature flags por-org que set-flag aceita (allowlist).
var adminFlags = map[string]bool{
	"graph_expansion": true, // liga a graph-expansion (5º canal RRF) só pra esta org
}

// adminSetFlag liga/desliga uma feature flag por-org em organizations.settings.
//
//	nexus admin set-flag <org_id> <flag> <true|false>
//
// Rollout seletivo: ex. ligar graph_expansion só nas orgs com grafo re-extraído.
// Roda contra ADMIN_DATABASE_URL (bypassa RLS pra alcançar qualquer org).
func adminSetFlag(args []string) error {
	if len(args) < 3 {
		return fmt.Errorf("uso: nexus admin set-flag <org_id> <flag> <true|false> (flags: graph_expansion)")
	}
	orgID, flag, val := args[0], args[1], args[2]
	if !adminFlags[flag] {
		return fmt.Errorf("flag não permitida: %q (disponível: graph_expansion)", flag)
	}
	if val != "true" && val != "false" {
		return fmt.Errorf("valor deve ser true|false, recebido %q", val)
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	db, err := sql.Open("pgx", cfg.DB.AdminOrDefault())
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = db.Close() }()

	res, err := db.ExecContext(context.Background(), `
		UPDATE organizations
		SET settings = COALESCE(settings, '{}'::jsonb) || jsonb_build_object($2::text, $3::boolean),
		    updated_at = now()
		WHERE id = $1::bigint`,
		orgID, flag, val == "true")
	if err != nil {
		return fmt.Errorf("update settings: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("org %s não encontrada", orgID)
	}
	fmt.Printf("org %s: %s = %s\n", orgID, flag, val)
	return nil
}

func adminCreateToken(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("uso: nexus admin create-token <slug> <name> [abilities-csv, default '*']")
	}
	slug, name := args[0], args[1]
	abilities := "*"
	if len(args) >= 3 && args[2] != "" {
		abilities = args[2]
	}
	// abilities CSV → array literal do Postgres: "a,b" → "{a,b}"
	abilArr := "{" + strings.Join(strings.Split(abilities, ","), ",") + "}"

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	db, err := sql.Open("pgx", cfg.DB.AdminOrDefault())
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = db.Close() }()

	// secret = 32 bytes random em hex; token_hash = sha256(secret). Mesmo esquema do auth.
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Errorf("rand: %w", err)
	}
	secret := hex.EncodeToString(raw)
	sum := sha256.Sum256([]byte(secret))
	hash := hex.EncodeToString(sum[:])

	// Idempotente: reusa a org se o slug já existe e emite um token novo (re-rodar
	// o comando não quebra — ver 0029). org_created distingue criou vs reusou.
	var orgID, tokenID int64
	var orgCreated bool
	err = db.QueryRowContext(context.Background(),
		`SELECT org_id, token_id, org_created FROM ensure_org_with_token($1, $2, $3, $4::text[])`,
		slug, name, hash, abilArr).Scan(&orgID, &tokenID, &orgCreated)
	if err != nil {
		return fmt.Errorf("ensure_org_with_token: %w", err)
	}

	// Aviso vai pro stderr pra não poluir o token (stdout) — facilita capturar em script.
	if !orgCreated {
		fmt.Fprintf(os.Stderr, "note: org %q already existed (id=%d) — issued a new token (older tokens still valid)\n", slug, orgID)
	}
	// O token completo (id|secret) só é mostrado AQUI — não fica recuperável depois.
	fmt.Printf("org_id=%d\ntoken=%d|%s\n", orgID, tokenID, secret)
	return nil
}
