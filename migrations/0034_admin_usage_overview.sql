-- 0034_admin_usage_overview.sql — visão agregada cross-org de cota vs consumo (admin).
-- SECURITY DEFINER (owner nexus_service/BYPASSRLS) pra ler TODAS as orgs num só round-trip,
-- juntando limites (org_limits), consumo de queries do mês (org_usage) e memórias
-- (count em pages). Backing do GET /v1/admin/usage — página de calibração de cota do
-- console (cota contratada vs. uso real por plano). 0 = ILIMITADO.

-- +goose Up
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION admin_usage_overview()
RETURNS TABLE (
    organization_id bigint,
    org_name        text,
    max_pages       bigint,
    max_queries     bigint,
    max_rps         integer,
    pages_used      bigint,
    queries_used    bigint
)
LANGUAGE sql
SECURITY DEFINER
SET search_path = public
AS $$
    SELECT o.id,
           o.name,
           coalesce(l.max_pages, 0),
           coalesce(l.max_queries, 0),
           coalesce(l.max_rps, 0),
           coalesce(p.cnt, 0),
           coalesce(u.used, 0)
    FROM organizations o
    LEFT JOIN org_limits l ON l.organization_id = o.id
    LEFT JOIN (SELECT organization_id, count(*) AS cnt FROM pages GROUP BY organization_id) p
           ON p.organization_id = o.id
    LEFT JOIN org_usage u ON u.organization_id = o.id
           AND u.metric = 'query'
           AND u.period = date_trunc('month', now())::date
    ORDER BY o.id;
$$;

ALTER FUNCTION admin_usage_overview() OWNER TO nexus_service;
REVOKE ALL ON FUNCTION admin_usage_overview() FROM PUBLIC;
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_app') THEN
        GRANT EXECUTE ON FUNCTION admin_usage_overview() TO nexus_app;
    END IF;
END $$;

COMMENT ON FUNCTION admin_usage_overview() IS
    'SECURITY DEFINER (owner nexus_service/BYPASSRLS): visão agregada cross-org de '
    'cota vs consumo (limites + queries do mês + memórias por org). Backing do '
    'GET /v1/admin/usage (calibração de cota). 0 = ilimitado.';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION IF EXISTS admin_usage_overview();
-- +goose StatementEnd
