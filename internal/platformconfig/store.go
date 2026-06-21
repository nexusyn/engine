// Package platformconfig persiste e lê a config de modelo GLOBAL do operador
// (admin-only) em platform_model_config: 1 linha por etapa (generation/
// extraction/embed/rerank) com provider + model + base_url + API key CIFRADA.
//
// Não confunde com orgconfig (por-org). Esta é global e não toca em organizations.
// A key é cifrada com secret.Cipher (AES-256-GCM, CONFIG_ENC_KEY) antes do
// Postgres e só é decifrada em Get (uso server-side na construção do provider).
package platformconfig

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nexusyn/engine/internal/secret"
)

// Stages válidos (espelha o CHECK da migration 0025).
var Stages = []string{"generation", "extraction", "embed", "rerank"}

// ValidStage diz se s é uma etapa conhecida.
func ValidStage(s string) bool {
	for _, v := range Stages {
		if v == s {
			return true
		}
	}
	return false
}

// Config é uma linha com a key JÁ DECIFRADA (uso server-side; nunca serializar
// pro cliente). APIKey vazia = usar a key do .env pro provider (back-compat).
type Config struct {
	Stage    string
	Provider string
	Model    string
	BaseURL  string
	APIKey   string
}

// View é a forma segura pra UI/API: key mascarada, sem plaintext.
type View struct {
	Stage    string `json:"stage"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	BaseURL  string `json:"base_url"`
	KeyMask  string `json:"key_mask"`
	HasKey   bool   `json:"has_key"`
}

// Load devolve todas as etapas configuradas, com a key MASCARADA (pra GET).
func Load(ctx context.Context, pool *pgxpool.Pool, c *secret.Cipher) ([]View, error) {
	rows, err := pool.Query(ctx, `
		SELECT stage, provider, model, base_url, api_key_enc
		FROM platform_model_config ORDER BY stage`)
	if err != nil {
		return nil, fmt.Errorf("platformconfig: load: %w", err)
	}
	defer rows.Close()

	var out []View
	for rows.Next() {
		var v View
		var enc string
		if err := rows.Scan(&v.Stage, &v.Provider, &v.Model, &v.BaseURL, &enc); err != nil {
			return nil, err
		}
		if enc != "" {
			v.HasKey = true
			if pt, derr := c.Decrypt(enc); derr == nil {
				v.KeyMask = secret.Mask(pt)
			} else {
				v.KeyMask = "••••(?)" // chave-mestra mudou? sinaliza sem vazar
			}
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Get devolve a config de UMA etapa com a key DECIFRADA (uso server-side).
// Retorna (nil, nil) se a etapa não está configurada.
func Get(ctx context.Context, pool *pgxpool.Pool, c *secret.Cipher, stage string) (*Config, error) {
	var cfg Config
	var enc string
	err := pool.QueryRow(ctx, `
		SELECT stage, provider, model, base_url, api_key_enc
		FROM platform_model_config WHERE stage = $1`, stage).
		Scan(&cfg.Stage, &cfg.Provider, &cfg.Model, &cfg.BaseURL, &enc)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("platformconfig: get %s: %w", stage, err)
	}
	if enc != "" {
		pt, derr := c.Decrypt(enc)
		if derr != nil {
			return nil, fmt.Errorf("platformconfig: decrypt %s: %w", stage, derr)
		}
		cfg.APIKey = pt
	}
	return &cfg, nil
}

// Set faz upsert de uma etapa. Se apiKey == "", PRESERVA a key já gravada
// (permite editar só o modelo sem recolar a key). Se apiKey != "", cifra e grava.
func Set(ctx context.Context, pool *pgxpool.Pool, c *secret.Cipher, cfg Config) error {
	if !ValidStage(cfg.Stage) {
		return fmt.Errorf("platformconfig: stage inválido: %q", cfg.Stage)
	}
	if cfg.Provider == "" {
		return fmt.Errorf("platformconfig: provider obrigatório")
	}

	if cfg.APIKey == "" {
		// Preserva a key existente; atualiza só provider/model/base_url.
		_, err := pool.Exec(ctx, `
			INSERT INTO platform_model_config (stage, provider, model, base_url, api_key_enc, updated_at)
			VALUES ($1, $2, $3, $4, '', NOW())
			ON CONFLICT (stage) DO UPDATE SET
				provider = EXCLUDED.provider,
				model    = EXCLUDED.model,
				base_url = EXCLUDED.base_url,
				updated_at = NOW()`,
			cfg.Stage, cfg.Provider, cfg.Model, cfg.BaseURL)
		return err
	}

	enc, err := c.Encrypt(cfg.APIKey)
	if err != nil {
		return fmt.Errorf("platformconfig: encrypt: %w", err)
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO platform_model_config (stage, provider, model, base_url, api_key_enc, updated_at)
		VALUES ($1, $2, $3, $4, $5, NOW())
		ON CONFLICT (stage) DO UPDATE SET
			provider = EXCLUDED.provider,
			model    = EXCLUDED.model,
			base_url = EXCLUDED.base_url,
			api_key_enc = EXCLUDED.api_key_enc,
			updated_at = NOW()`,
		cfg.Stage, cfg.Provider, cfg.Model, cfg.BaseURL, enc)
	return err
}
