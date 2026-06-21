-- 0016_org_model_config.sql — Config de modelo por organização, por etapa
-- (Fase 0 do dashboard). Override do LLMConfig global (.env) por org:
-- generation / extraction / embed / rerank.
--
-- v1 cobre seleção de provider + model + base_url. BYO api-key (criptografada)
-- fica pra increment seguinte (secret-at-rest exige encryption adequada).
--
-- Resolução: o query/extract path lê esta config (org override > default
-- global) e constrói o provider correspondente. 1 row por (org, stage).

-- +goose Up
-- +goose StatementBegin

CREATE TABLE org_model_config (
    id              BIGSERIAL PRIMARY KEY,
    organization_id BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    stage           TEXT NOT NULL,
    provider        TEXT NOT NULL,
    model           TEXT NOT NULL DEFAULT '',
    base_url        TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(organization_id, stage),
    CONSTRAINT org_model_config_stage_chk
        CHECK (stage IN ('generation','extraction','embed','rerank'))
);

CREATE INDEX org_model_config_org_idx ON org_model_config(organization_id);

ALTER TABLE org_model_config ENABLE ROW LEVEL SECURITY;
ALTER TABLE org_model_config FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON org_model_config
    USING (organization_id = current_org_id())
    WITH CHECK (organization_id = current_org_id());

COMMENT ON TABLE org_model_config IS
    'Override per-org do modelo por etapa (generation/extraction/embed/rerank). '
    'Ausência de row = usa o default global do LLMConfig (.env).';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP POLICY IF EXISTS tenant_isolation ON org_model_config;
ALTER TABLE org_model_config DISABLE ROW LEVEL SECURITY;
DROP TABLE IF EXISTS org_model_config;

-- +goose StatementEnd
