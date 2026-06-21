-- 0023_compile_state.sql — watermark do auto-compile incremental (worker).
--
-- O compile automático (River PeriodicJob, gated por COMPILE_INTERVAL>0) processa
-- só as fontes (memory/knowledge) NOVAS desde o último compile de cada org, em
-- lotes limitados (COMPILE_MAX_ITEMS_PER_RUN). compile_state guarda, por org, o
-- created_at da fonte mais recente já compilada. Funções SECURITY DEFINER (owner
-- nexus_service/BYPASSRLS) leem/escrevem cross-org — mesmo padrão do 0020/0018.

-- +goose Up
-- +goose StatementBegin

CREATE TABLE IF NOT EXISTS compile_state (
    organization_id  BIGINT PRIMARY KEY REFERENCES organizations(id) ON DELETE CASCADE,
    last_compiled_at TIMESTAMPTZ NOT NULL DEFAULT 'epoch',
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE compile_state ENABLE ROW LEVEL SECURITY;
ALTER TABLE compile_state FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON compile_state;
CREATE POLICY tenant_isolation ON compile_state
    USING (organization_id = current_org_id())
    WITH CHECK (organization_id = current_org_id());

GRANT SELECT ON compile_state TO nexus_app;
GRANT SELECT, INSERT, UPDATE ON compile_state TO nexus_service;

-- list_orgs_needing_compile: orgs com ≥1 fonte (memory/knowledge) viva criada
-- DEPOIS do watermark. p_limit limita quantas orgs por tick (anti-contenção da
-- key MiniMax compartilhada). Cross-org → SECURITY DEFINER.
CREATE OR REPLACE FUNCTION list_orgs_needing_compile(p_limit integer)
RETURNS TABLE(organization_id bigint)
LANGUAGE sql
SECURITY DEFINER
SET search_path = public
AS $$
    SELECT p.organization_id
    FROM pages p
    LEFT JOIN compile_state cs ON cs.organization_id = p.organization_id
    WHERE p.valid_to IS NULL
      AND p.domain IN ('memory','knowledge')
      AND p.created_at > COALESCE(cs.last_compiled_at, 'epoch'::timestamptz)
    GROUP BY p.organization_id
    ORDER BY p.organization_id
    LIMIT p_limit;
$$;

CREATE OR REPLACE FUNCTION get_compile_watermark(p_org bigint)
RETURNS timestamptz
LANGUAGE sql
SECURITY DEFINER
SET search_path = public
AS $$
    SELECT COALESCE(
        (SELECT last_compiled_at FROM compile_state WHERE organization_id = p_org),
        'epoch'::timestamptz);
$$;

CREATE OR REPLACE FUNCTION set_compile_watermark(p_org bigint, p_ts timestamptz)
RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public
AS $$
BEGIN
    INSERT INTO compile_state (organization_id, last_compiled_at, updated_at)
    VALUES (p_org, p_ts, now())
    ON CONFLICT (organization_id) DO UPDATE
        SET last_compiled_at = GREATEST(compile_state.last_compiled_at, EXCLUDED.last_compiled_at),
            updated_at       = now();
END;
$$;

ALTER FUNCTION list_orgs_needing_compile(integer) OWNER TO nexus_service;
ALTER FUNCTION get_compile_watermark(bigint) OWNER TO nexus_service;
ALTER FUNCTION set_compile_watermark(bigint, timestamptz) OWNER TO nexus_service;
REVOKE ALL ON FUNCTION list_orgs_needing_compile(integer) FROM PUBLIC;
REVOKE ALL ON FUNCTION get_compile_watermark(bigint) FROM PUBLIC;
REVOKE ALL ON FUNCTION set_compile_watermark(bigint, timestamptz) FROM PUBLIC;
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_app') THEN
        GRANT EXECUTE ON FUNCTION list_orgs_needing_compile(integer)    TO nexus_app;
        GRANT EXECUTE ON FUNCTION get_compile_watermark(bigint)         TO nexus_app;
        GRANT EXECUTE ON FUNCTION set_compile_watermark(bigint, timestamptz) TO nexus_app;
    END IF;
END $$;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION IF EXISTS set_compile_watermark(bigint, timestamptz);
DROP FUNCTION IF EXISTS get_compile_watermark(bigint);
DROP FUNCTION IF EXISTS list_orgs_needing_compile(integer);
DROP TABLE IF EXISTS compile_state;
-- +goose StatementEnd
