// Package orgconfig persiste e resolve a config de modelo por organização,
// por etapa (generation/extraction/embed/rerank). É o override per-org do
// LLMConfig global (.env) — base da feature "config de modelo por etapa" do
// dashboard (Fase 0).
package orgconfig

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nexusyn/engine/internal/tenant"
)

// Stage é uma etapa do pipeline com modelo configurável.
type Stage string

const (
	StageGeneration Stage = "generation"
	StageExtraction Stage = "extraction"
	StageEmbed      Stage = "embed"
	StageRerank     Stage = "rerank"
)

// ValidStage reporta se s é uma etapa conhecida.
func ValidStage(s string) bool {
	switch Stage(s) {
	case StageGeneration, StageExtraction, StageEmbed, StageRerank:
		return true
	}
	return false
}

// ModelChoice é a escolha de provider/model pra uma etapa.
type ModelChoice struct {
	Stage    string `json:"stage"`
	Provider string `json:"provider"`
	Model    string `json:"model,omitempty"`
	BaseURL  string `json:"base_url,omitempty"`
}

// Validate checa campos obrigatórios antes de persistir.
func (c ModelChoice) Validate() error {
	if !ValidStage(c.Stage) {
		return fmt.Errorf("stage inválido: %q (use generation|extraction|embed|rerank)", c.Stage)
	}
	if c.Provider == "" {
		return fmt.Errorf("provider é obrigatório")
	}
	return nil
}

// Load retorna as etapas configuradas da org (vazio = usa defaults globais).
func Load(ctx context.Context, pool *pgxpool.Pool, orgID int64) ([]ModelChoice, error) {
	var out []ModelChoice
	err := tenant.RunWithTenantReadOnly(ctx, pool, orgID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT stage, provider, model, base_url
			 FROM org_model_config WHERE organization_id = $1 ORDER BY stage`, orgID)
		if err != nil {
			return fmt.Errorf("orgconfig load: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var c ModelChoice
			if err := rows.Scan(&c.Stage, &c.Provider, &c.Model, &c.BaseURL); err != nil {
				return fmt.Errorf("orgconfig scan: %w", err)
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, err
}

// Upsert grava/atualiza a escolha de uma etapa (1 row por org+stage).
func Upsert(ctx context.Context, pool *pgxpool.Pool, orgID int64, c ModelChoice) error {
	if err := c.Validate(); err != nil {
		return err
	}
	return tenant.RunWithTenant(ctx, pool, orgID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO org_model_config (organization_id, stage, provider, model, base_url, updated_at)
			VALUES ($1, $2, $3, $4, $5, NOW())
			ON CONFLICT (organization_id, stage) DO UPDATE SET
			    provider   = EXCLUDED.provider,
			    model      = EXCLUDED.model,
			    base_url   = EXCLUDED.base_url,
			    updated_at = NOW()
		`, orgID, c.Stage, c.Provider, c.Model, c.BaseURL)
		if err != nil {
			return fmt.Errorf("orgconfig upsert: %w", err)
		}
		return nil
	})
}
