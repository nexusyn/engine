-- 0008_rls_policies.sql — Row-Level Security em todas tabelas multi-tenant
--
-- Padrão crítico: current_setting('nexus.org_id', true) com missing_ok=TRUE.
-- Sem o `true`, queries em rotas sem tenant bind (ex: /health, admin) crasham.
-- NULLIF + cast pra bigint: protege contra string vazia.
--
-- IMPORTANTE: aplicação Go DEVE usar `SET LOCAL nexus.org_id` dentro de transação
-- (ver internal/tenant/run.go::RunWithTenant). NUNCA SET sem LOCAL — vaza no pool.

-- +goose Up
-- +goose StatementBegin

-- ───── Helper function ─────
-- Retorna org_id atual da sessão como bigint, ou NULL se não setado.
-- Centraliza a lógica pra evitar repetir NULLIF+cast em cada policy.
CREATE OR REPLACE FUNCTION current_org_id() RETURNS BIGINT AS $$
    SELECT NULLIF(current_setting('nexus.org_id', true), '')::BIGINT;
$$ LANGUAGE SQL STABLE;

-- ───── Enable RLS em todas tabelas ─────
-- IMPORTANTE: `FORCE ROW LEVEL SECURITY` é necessário porque o usuário `nexus`
-- é OWNER das tabelas, e owners BYPASSAM RLS por default. Sem FORCE, a aplicação
-- (que conecta como `nexus`) ignoraria todas as policies.
-- Só a role `nexus_service` (com BYPASSRLS explícito) pode escapar.
ALTER TABLE organizations  ENABLE ROW LEVEL SECURITY; ALTER TABLE organizations  FORCE ROW LEVEL SECURITY;
ALTER TABLE agents          ENABLE ROW LEVEL SECURITY; ALTER TABLE agents         FORCE ROW LEVEL SECURITY;
ALTER TABLE api_tokens      ENABLE ROW LEVEL SECURITY; ALTER TABLE api_tokens     FORCE ROW LEVEL SECURITY;
ALTER TABLE pages           ENABLE ROW LEVEL SECURITY; ALTER TABLE pages          FORCE ROW LEVEL SECURITY;
ALTER TABLE sessions        ENABLE ROW LEVEL SECURITY; ALTER TABLE sessions       FORCE ROW LEVEL SECURITY;
ALTER TABLE messages        ENABLE ROW LEVEL SECURITY; ALTER TABLE messages       FORCE ROW LEVEL SECURITY;
ALTER TABLE entities        ENABLE ROW LEVEL SECURITY; ALTER TABLE entities       FORCE ROW LEVEL SECURITY;
ALTER TABLE edges           ENABLE ROW LEVEL SECURITY; ALTER TABLE edges          FORCE ROW LEVEL SECURITY;
ALTER TABLE lessons         ENABLE ROW LEVEL SECURITY; ALTER TABLE lessons        FORCE ROW LEVEL SECURITY;
ALTER TABLE decisions       ENABLE ROW LEVEL SECURITY; ALTER TABLE decisions      FORCE ROW LEVEL SECURITY;
ALTER TABLE events          ENABLE ROW LEVEL SECURITY; ALTER TABLE events         FORCE ROW LEVEL SECURITY;
ALTER TABLE usage_records   ENABLE ROW LEVEL SECURITY; ALTER TABLE usage_records  FORCE ROW LEVEL SECURITY;

-- ───── Policies ─────
-- organizations: lê própria org
CREATE POLICY org_isolation ON organizations
    USING (id = current_org_id());

-- Demais tabelas: filtram por organization_id
CREATE POLICY tenant_isolation ON agents
    USING (organization_id = current_org_id());

CREATE POLICY tenant_isolation ON api_tokens
    USING (organization_id = current_org_id());

CREATE POLICY tenant_isolation ON pages
    USING (organization_id = current_org_id());

CREATE POLICY tenant_isolation ON sessions
    USING (organization_id = current_org_id());

CREATE POLICY tenant_isolation ON messages
    USING (organization_id = current_org_id());

CREATE POLICY tenant_isolation ON entities
    USING (organization_id = current_org_id());

CREATE POLICY tenant_isolation ON edges
    USING (organization_id = current_org_id());

CREATE POLICY tenant_isolation ON lessons
    USING (organization_id = current_org_id());

CREATE POLICY tenant_isolation ON decisions
    USING (organization_id = current_org_id());

CREATE POLICY tenant_isolation ON events
    USING (organization_id = current_org_id());

CREATE POLICY tenant_isolation ON usage_records
    USING (organization_id = current_org_id());

-- ───── Service role (bypass RLS pra admin/migrations) ─────
-- Criamos role explícita pra operações que precisam ver across tenants
-- (admin, bench reset, migrations, jobs sistêmicos).
-- O user da aplicação NUNCA usa essa role — só admin CLI explícito.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_service') THEN
        CREATE ROLE nexus_service BYPASSRLS NOLOGIN;
    END IF;
END$$;

-- ───── Comentário pra futuro debug ─────
COMMENT ON FUNCTION current_org_id() IS
    'Retorna org_id da sessão Postgres como bigint. NULL se não setado. '
    'Aplicação Go usa SET LOCAL nexus.org_id = N dentro de transação. '
    'NUNCA usar SET sem LOCAL — vaza tenant entre connections do pool.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP POLICY IF EXISTS tenant_isolation ON usage_records;
DROP POLICY IF EXISTS tenant_isolation ON events;
DROP POLICY IF EXISTS tenant_isolation ON decisions;
DROP POLICY IF EXISTS tenant_isolation ON lessons;
DROP POLICY IF EXISTS tenant_isolation ON edges;
DROP POLICY IF EXISTS tenant_isolation ON entities;
DROP POLICY IF EXISTS tenant_isolation ON messages;
DROP POLICY IF EXISTS tenant_isolation ON sessions;
DROP POLICY IF EXISTS tenant_isolation ON pages;
DROP POLICY IF EXISTS tenant_isolation ON api_tokens;
DROP POLICY IF EXISTS tenant_isolation ON agents;
DROP POLICY IF EXISTS org_isolation ON organizations;

ALTER TABLE usage_records   DISABLE ROW LEVEL SECURITY;
ALTER TABLE events          DISABLE ROW LEVEL SECURITY;
ALTER TABLE decisions       DISABLE ROW LEVEL SECURITY;
ALTER TABLE lessons         DISABLE ROW LEVEL SECURITY;
ALTER TABLE edges           DISABLE ROW LEVEL SECURITY;
ALTER TABLE entities        DISABLE ROW LEVEL SECURITY;
ALTER TABLE messages        DISABLE ROW LEVEL SECURITY;
ALTER TABLE sessions        DISABLE ROW LEVEL SECURITY;
ALTER TABLE pages           DISABLE ROW LEVEL SECURITY;
ALTER TABLE api_tokens      DISABLE ROW LEVEL SECURITY;
ALTER TABLE agents          DISABLE ROW LEVEL SECURITY;
ALTER TABLE organizations   DISABLE ROW LEVEL SECURITY;

DROP FUNCTION IF EXISTS current_org_id();
DROP ROLE IF EXISTS nexus_service;

-- +goose StatementEnd
