-- 0020_org_limits.sql — quota de plano por org (control-plane).
--
-- max_pages = memórias; max_queries = queries/mês; max_rps. 0 = ILIMITADO.
-- Setado pelo console (SyncTenantPlan) via PUT /v1/admin/orgs/{id}/limits ->
-- set_org_limits (SECURITY DEFINER, escreve cross-org bypassando RLS — mesmo
-- padrão do create_org_with_token / 0018). Lido pelo ingest pra enforçar storage,
-- gated por NEXUS_ENFORCE_LIMITS (default OFF → enforcement desligado).

-- +goose Up
-- +goose StatementBegin

CREATE TABLE IF NOT EXISTS org_limits (
    organization_id BIGINT PRIMARY KEY REFERENCES organizations(id) ON DELETE CASCADE,
    max_pages   BIGINT  NOT NULL DEFAULT 0,
    max_queries BIGINT  NOT NULL DEFAULT 0,
    max_rps     INTEGER NOT NULL DEFAULT 0,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE org_limits ENABLE ROW LEVEL SECURITY;
ALTER TABLE org_limits FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON org_limits;
CREATE POLICY tenant_isolation ON org_limits
    USING (organization_id = current_org_id())
    WITH CHECK (organization_id = current_org_id());

-- app role lê o limite da PRÓPRIA org (ingest); nexus_service escreve qualquer org.
GRANT SELECT ON org_limits TO nexus_app;
GRANT SELECT, INSERT, UPDATE ON org_limits TO nexus_service;

CREATE OR REPLACE FUNCTION set_org_limits(
    p_org bigint, p_max_pages bigint, p_max_queries bigint, p_max_rps integer
)
RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public
AS $$
BEGIN
    INSERT INTO org_limits (organization_id, max_pages, max_queries, max_rps, updated_at)
    VALUES (p_org, p_max_pages, p_max_queries, p_max_rps, now())
    ON CONFLICT (organization_id) DO UPDATE
        SET max_pages   = EXCLUDED.max_pages,
            max_queries = EXCLUDED.max_queries,
            max_rps     = EXCLUDED.max_rps,
            updated_at  = now();
END;
$$;

ALTER FUNCTION set_org_limits(bigint, bigint, bigint, integer) OWNER TO nexus_service;
REVOKE ALL ON FUNCTION set_org_limits(bigint, bigint, bigint, integer) FROM PUBLIC;
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_app') THEN
        GRANT EXECUTE ON FUNCTION set_org_limits(bigint, bigint, bigint, integer) TO nexus_app;
    END IF;
END $$;

COMMENT ON FUNCTION set_org_limits(bigint, bigint, bigint, integer) IS
    'SECURITY DEFINER (owner nexus_service/BYPASSRLS): upsert dos limites de quota '
    'de uma org. Control-plane (PUT /v1/admin/orgs/{id}/limits), chamado pelo console '
    'com MASTER token (ability admin).';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION IF EXISTS set_org_limits(bigint, bigint, bigint, integer);
DROP TABLE IF EXISTS org_limits;
-- +goose StatementEnd
