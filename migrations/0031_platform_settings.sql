-- 0031_platform_settings.sql — config GLOBAL da plataforma (key-value), NÃO multi-tenant
-- (sem organization_id → sem RLS). Primeiro uso: 'enforce_limits' — o toggle de cota do
-- admin (botão no console), lido em runtime pelo engine. BETA: linha ausente = OFF.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS platform_settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_app') THEN
        GRANT SELECT, INSERT, UPDATE ON platform_settings TO nexus_app;
    END IF;
END $$;
COMMENT ON TABLE platform_settings IS
    'Config global da plataforma (key-value), não multi-tenant. Ex: enforce_limits (toggle de cota do admin, runtime).';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS platform_settings;
-- +goose StatementEnd
