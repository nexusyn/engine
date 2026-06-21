-- 0001_tenancy.sql — Organizations + Agents (raíz do multi-tenant)
--
-- Por que primeiro: todas outras tabelas têm FK em organizations.
-- Schema bi-temporal: organizations e agents não são mutáveis o suficiente pra
-- justificar valid_from/valid_to (são metadata estável). Só created_at/updated_at.

-- +goose Up
-- +goose StatementBegin

CREATE TABLE organizations (
    id          BIGSERIAL PRIMARY KEY,
    slug        TEXT NOT NULL UNIQUE,
    name        TEXT NOT NULL,
    settings    JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX organizations_slug_idx ON organizations(slug);

CREATE TABLE agents (
    id              BIGSERIAL PRIMARY KEY,
    organization_id BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    slug            TEXT NOT NULL,
    name            TEXT NOT NULL,
    persona         JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    UNIQUE (organization_id, slug)
);

CREATE INDEX agents_org_idx ON agents(organization_id);

-- updated_at trigger genérico (reusado em outras tabelas)
CREATE OR REPLACE FUNCTION set_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER organizations_set_updated_at
    BEFORE UPDATE ON organizations
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER agents_set_updated_at
    BEFORE UPDATE ON agents
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS agents_set_updated_at ON agents;
DROP TRIGGER IF EXISTS organizations_set_updated_at ON organizations;
DROP FUNCTION IF EXISTS set_updated_at();
DROP TABLE IF EXISTS agents;
DROP TABLE IF EXISTS organizations;
-- +goose StatementEnd
