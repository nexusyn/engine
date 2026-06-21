-- 0014_user_profiles.sql — Sintetiza preferences/lessons em documento markdown
-- consultável (Sprint 3.1).
--
-- Filosofia: extract entities é fino (1 fato → 1 entity); query precisa de
-- contexto agregado (perfil coeso). Job assíncrono lê entities kind ∈
-- {preference, lesson} da org, chama LLM, escreve markdown estruturado aqui.
--
-- Read path: /v1/query injeta este blob em queries detected como
-- preference-flavored (IsPreferenceQuery), junto com KNOWN PREFERENCES.
--
-- 1 row por organization (UNIQUE) — v1 trata cada org como single user. Multi-
-- user dentro da mesma org fica pra v2 quando agents tiverem profile próprio.

-- +goose Up
-- +goose StatementBegin

CREATE TABLE user_profiles (
    id BIGSERIAL PRIMARY KEY,
    organization_id BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    content TEXT NOT NULL DEFAULT '',
    preferences_count INT NOT NULL DEFAULT 0,
    lessons_count INT NOT NULL DEFAULT 0,
    built_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(organization_id)
);

CREATE INDEX user_profiles_built_at_idx ON user_profiles(built_at);

-- RLS — mesma policy de tenant isolation
ALTER TABLE user_profiles ENABLE ROW LEVEL SECURITY;
ALTER TABLE user_profiles FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON user_profiles
    USING (organization_id = current_org_id())
    WITH CHECK (organization_id = current_org_id());

COMMENT ON TABLE user_profiles IS
    'Sintetiza preferences/lessons em markdown consultável. BuildUserProfileJob '
    'atualiza periodicamente (River PeriodicJob 1h). Read em /v1/query.';

COMMENT ON COLUMN user_profiles.content IS
    'Markdown estruturado pelo LLM. Tipicamente 200-2000 tokens. Vazio quando '
    'org não tem preferences/lessons ainda.';

COMMENT ON COLUMN user_profiles.built_at IS
    'Última vez que BuildUserProfileJob escreveu este profile. '
    'Stale > 1h indica scheduler parado ou erro no job.';

-- ───── SECURITY DEFINER helper pro scheduler ─────
-- PeriodicJob roda sem tenant bind (não é uma request HTTP); precisa enumerar
-- orgs com preferences pra agendar 1 BuildUserProfileJob por org.
-- Same pattern do Day 14 (drain_pending_chunks): owner é nexus_service
-- (BYPASSRLS), worker chama como nexus_app sem precisar elevar privilege.

CREATE OR REPLACE FUNCTION list_orgs_with_profile_facts()
RETURNS TABLE(organization_id BIGINT)
LANGUAGE SQL
SECURITY DEFINER
SET search_path = public
AS $$
    SELECT DISTINCT e.organization_id
    FROM entities e
    WHERE e.kind IN ('preference', 'lesson')
    ORDER BY e.organization_id;
$$;

ALTER FUNCTION list_orgs_with_profile_facts() OWNER TO nexus_service;
REVOKE ALL ON FUNCTION list_orgs_with_profile_facts() FROM PUBLIC;
-- nexus_app é criado pelo postgres-init.sh ANTES das migrations em prod, mas nos
-- testes de integração (testcontainers) ele é criado DEPOIS do goose.Up. Guardamos
-- o GRANT pra a migration ser portável: em prod o role existe → concede normal; no
-- teste o role ainda não existe → no-op (o createAppUser concede o que precisa depois).
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_app') THEN
        GRANT EXECUTE ON FUNCTION list_orgs_with_profile_facts() TO nexus_app;
    END IF;
END $$;

COMMENT ON FUNCTION list_orgs_with_profile_facts() IS
    'SECURITY DEFINER: cross-tenant enumeração de orgs com preference/lesson '
    'entities. Chamada pelo PeriodicJob do scheduler. Owner = nexus_service '
    '(BYPASSRLS). Caller = nexus_app (sem elevação de privilégio).';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP FUNCTION IF EXISTS list_orgs_with_profile_facts();
DROP POLICY IF EXISTS tenant_isolation ON user_profiles;
ALTER TABLE user_profiles DISABLE ROW LEVEL SECURITY;
DROP TABLE IF EXISTS user_profiles;

-- +goose StatementEnd
