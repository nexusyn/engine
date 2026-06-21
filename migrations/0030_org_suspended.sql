-- 0030_org_suspended.sql — suspensão de org (billing). Bloqueia o data-plane
-- (query/search/ingest) de uma org SEM apagar dados. AUD-010: o console já chamava
-- PATCH /v1/admin/orgs/{id}/suspend, mas o engine não implementava o endpoint nem o
-- gate → org inadimplente/suspensa seguia operando (bypass de cobrança). Mesmo padrão
-- SECURITY DEFINER de set_org_limits. suspended_at NULL = ativa.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE organizations ADD COLUMN IF NOT EXISTS suspended_at TIMESTAMPTZ;

CREATE OR REPLACE FUNCTION set_org_suspended(p_org bigint, p_suspended boolean)
RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public
AS $$
BEGIN
    UPDATE organizations
       SET suspended_at = CASE WHEN p_suspended THEN now() ELSE NULL END
     WHERE id = p_org;
END;
$$;
ALTER FUNCTION set_org_suspended(bigint, boolean) OWNER TO nexus_service;
REVOKE ALL ON FUNCTION set_org_suspended(bigint, boolean) FROM PUBLIC;

CREATE OR REPLACE FUNCTION is_org_suspended(p_org bigint)
RETURNS boolean
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = public
AS $$
    SELECT COALESCE((SELECT suspended_at IS NOT NULL FROM organizations WHERE id = p_org), false);
$$;
ALTER FUNCTION is_org_suspended(bigint) OWNER TO nexus_service;
REVOKE ALL ON FUNCTION is_org_suspended(bigint) FROM PUBLIC;

DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_app') THEN
        GRANT EXECUTE ON FUNCTION set_org_suspended(bigint, boolean) TO nexus_app;
        GRANT EXECUTE ON FUNCTION is_org_suspended(bigint) TO nexus_app;
    END IF;
END $$;

COMMENT ON COLUMN organizations.suspended_at IS
    'Billing: org suspensa (data-plane query/search/ingest bloqueado). NULL = ativa. AUD-010.';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION IF EXISTS is_org_suspended(bigint);
DROP FUNCTION IF EXISTS set_org_suspended(bigint, boolean);
ALTER TABLE organizations DROP COLUMN IF EXISTS suspended_at;
-- +goose StatementEnd
