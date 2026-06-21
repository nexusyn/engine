-- 0025_platform_model_config.sql — config de modelo GLOBAL da plataforma (admin-only).
--
-- Diferente de org_model_config (por-org): esta é a config do OPERADOR, uma linha
-- por etapa (generation/extraction/embed/rerank), válida pra plataforma toda.
-- Substitui a edição manual do .env: provider + model + base_url + a API key do
-- provider CIFRADA (AES-GCM no engine, chave-mestra CONFIG_ENC_KEY).
--
-- NÃO referencia organizations — é global. NÃO mexe em org_model_config.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE platform_model_config (
    stage         TEXT PRIMARY KEY
                  CHECK (stage IN ('generation','extraction','embed','rerank')),
    provider      TEXT NOT NULL,
    model         TEXT NOT NULL DEFAULT '',
    base_url      TEXT NOT NULL DEFAULT '',
    -- API key cifrada (base64 de nonce||ciphertext, AES-256-GCM). NUNCA plaintext.
    -- Vazia = usar a key do .env (back-compat) pro provider.
    api_key_enc   TEXT NOT NULL DEFAULT '',
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE platform_model_config IS
    'Config de modelo global do operador (admin-only). 1 linha por etapa. '
    'api_key_enc = AES-256-GCM (CONFIG_ENC_KEY). Não confundir com org_model_config (por-org).';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS platform_model_config;
-- +goose StatementEnd
