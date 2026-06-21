-- 0027_org_usage.sql — contador mensal de uso por org (enforcement de quota).
--
-- usage_records é o LEDGER (1 linha por evento, auditoria/relatório); org_usage
-- é o CONTADOR (1 linha por org+período+métrica) pro check de quota no caminho
-- quente — sem COUNT(*) por request. Incrementado/checado atomicamente por
-- check_and_incr_usage (SECURITY DEFINER, padrão set_org_limits/0020), que lê
-- o limite de org_limits (max_queries pra metric='query'). 0 = ILIMITADO.
--
-- O contador SEMPRE incrementa (observabilidade); o bloqueio (allowed=false,
-- sem incremento) só acontece com p_enforce=true — controlado no engine pelo
-- kill-switch NEXUS_ENFORCE_LIMITS (default OFF).

-- +goose Up
-- +goose StatementBegin

CREATE TABLE IF NOT EXISTS org_usage (
    organization_id BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    period      DATE   NOT NULL, -- 1º dia do mês (date_trunc('month', now()))
    metric      TEXT   NOT NULL, -- 'query' (caminhos de retrieval: query/stream/search/MCP)
    used        BIGINT NOT NULL DEFAULT 0,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (organization_id, period, metric)
);

ALTER TABLE org_usage ENABLE ROW LEVEL SECURITY;
ALTER TABLE org_usage FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON org_usage;
CREATE POLICY tenant_isolation ON org_usage
    USING (organization_id = current_org_id())
    WITH CHECK (organization_id = current_org_id());

-- app role lê o consumo da PRÓPRIA org (/v1/usage); escrita só via function.
GRANT SELECT ON org_usage TO nexus_app;
GRANT SELECT, INSERT, UPDATE ON org_usage TO nexus_service;

CREATE OR REPLACE FUNCTION check_and_incr_usage(
    p_org bigint, p_metric text, p_enforce boolean
)
RETURNS TABLE (allowed boolean, used bigint, max_allowed bigint)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public
AS $$
DECLARE
    v_period date := date_trunc('month', now())::date;
    v_max    bigint := 0;
    v_used   bigint;
BEGIN
    -- Limite vigente da org pra métrica (0/sem linha = ilimitado).
    SELECT CASE p_metric WHEN 'query' THEN ol.max_queries ELSE 0 END
      INTO v_max FROM org_limits ol WHERE ol.organization_id = p_org;
    v_max := coalesce(v_max, 0);

    -- Garante a linha do período e trava (serializa o check+incr por org).
    INSERT INTO org_usage (organization_id, period, metric, used)
    VALUES (p_org, v_period, p_metric, 0)
    ON CONFLICT (organization_id, period, metric) DO NOTHING;

    SELECT ou.used INTO v_used FROM org_usage ou
    WHERE ou.organization_id = p_org AND ou.period = v_period AND ou.metric = p_metric
    FOR UPDATE;

    IF p_enforce AND v_max > 0 AND v_used >= v_max THEN
        RETURN QUERY SELECT false, v_used, v_max; -- bloqueado: NÃO incrementa
        RETURN;
    END IF;

    UPDATE org_usage ou SET used = ou.used + 1, updated_at = now()
    WHERE ou.organization_id = p_org AND ou.period = v_period AND ou.metric = p_metric
    RETURNING ou.used INTO v_used;

    RETURN QUERY SELECT true, v_used, v_max;
END;
$$;

ALTER FUNCTION check_and_incr_usage(bigint, text, boolean) OWNER TO nexus_service;
REVOKE ALL ON FUNCTION check_and_incr_usage(bigint, text, boolean) FROM PUBLIC;
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_app') THEN
        GRANT EXECUTE ON FUNCTION check_and_incr_usage(bigint, text, boolean) TO nexus_app;
    END IF;
END $$;

COMMENT ON FUNCTION check_and_incr_usage(bigint, text, boolean) IS
    'SECURITY DEFINER (owner nexus_service/BYPASSRLS): incrementa o contador '
    'mensal org_usage e checa contra org_limits (max_queries). p_enforce=false '
    '→ só conta (observação); p_enforce=true + estouro → allowed=false sem '
    'incrementar. Chamado nos caminhos de retrieval (query/stream/search/MCP).';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION IF EXISTS check_and_incr_usage(bigint, text, boolean);
DROP TABLE IF EXISTS org_usage;
-- +goose StatementEnd
