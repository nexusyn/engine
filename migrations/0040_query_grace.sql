-- 0040_query_grace.sql — bloqueio de queries 72h depois de estourar a cota.
--
-- A partir de 2026-10-01 o engine chama check_and_incr_query_grace com
-- p_grace_active=true. O relógio (quota_over_since) NÃO arma no estouro:
-- o console grava esse instante com arm_quota_notice no momento em que o
-- e-mail sai. Consultas novas só recebem 402 quando
-- now() >= quota_over_since + 72h e a cota continua estourada.
-- pages_over marca estouro de memórias (o console liga/desliga) e não arma
-- o relógio. Voltar pra dentro das duas cotas limpa o relógio.
-- check_and_incr_usage (0027) permanece o bloqueio imediato do kill-switch.

-- +goose Up
-- +goose StatementBegin

ALTER TABLE org_limits
    ADD COLUMN IF NOT EXISTS quota_over_since timestamptz,
    ADD COLUMN IF NOT EXISTS pages_over boolean NOT NULL DEFAULT false;

CREATE OR REPLACE FUNCTION check_and_incr_query_grace(
    p_org bigint,
    p_hard boolean,
    p_grace_active boolean,
    p_grace_hours int
)
RETURNS TABLE (
    allowed boolean,
    used bigint,
    max_allowed bigint,
    over_since timestamptz,
    block_at timestamptz
)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public
AS $$
DECLARE
    v_period date := date_trunc('month', now())::date;
    v_max bigint := 0;
    v_used bigint := 0;
    v_pages_over boolean := false;
    v_over timestamptz;
    v_grace interval := make_interval(hours => coalesce(p_grace_hours, 72));
BEGIN
    SELECT coalesce(ol.max_queries, 0), coalesce(ol.pages_over, false), ol.quota_over_since
      INTO v_max, v_pages_over, v_over
      FROM org_limits ol
     WHERE ol.organization_id = p_org;
    v_max := coalesce(v_max, 0);
    v_pages_over := coalesce(v_pages_over, false);

    INSERT INTO org_usage (organization_id, period, metric, used)
    VALUES (p_org, v_period, 'query', 0)
    ON CONFLICT (organization_id, period, metric) DO NOTHING;

    SELECT ou.used INTO v_used FROM org_usage ou
    WHERE ou.organization_id = p_org AND ou.period = v_period AND ou.metric = 'query'
    FOR UPDATE;
    v_used := coalesce(v_used, 0);

    -- Kill-switch antigo: estouro de queries bloqueia na hora.
    IF p_hard AND v_max > 0 AND v_used >= v_max THEN
        RETURN QUERY SELECT false, v_used, v_max, v_over,
            CASE WHEN v_over IS NULL THEN NULL ELSE v_over + v_grace END;
        RETURN;
    END IF;

    -- E-mail já enviado e as 72h desde esse e-mail venceram → 402, sem contar esta.
    IF p_grace_active AND v_over IS NOT NULL AND now() >= v_over + v_grace
       AND ((v_max > 0 AND v_used >= v_max) OR v_pages_over) THEN
        RETURN QUERY SELECT false, v_used, v_max, v_over, v_over + v_grace;
        RETURN;
    END IF;

    UPDATE org_usage ou SET used = ou.used + 1, updated_at = now()
    WHERE ou.organization_id = p_org AND ou.period = v_period AND ou.metric = 'query'
    RETURNING ou.used INTO v_used;

    -- O estouro não arma o relógio. Só a volta pra dentro das duas cotas limpa.
    IF (v_max = 0 OR v_used < v_max) AND NOT v_pages_over THEN
        IF v_over IS NOT NULL THEN
            UPDATE org_limits SET quota_over_since = NULL WHERE organization_id = p_org;
            v_over := NULL;
        END IF;
    END IF;

    RETURN QUERY SELECT true, v_used, v_max, v_over,
        CASE WHEN v_over IS NULL THEN NULL ELSE v_over + v_grace END;
END;
$$;

ALTER FUNCTION check_and_incr_query_grace(bigint, boolean, boolean, int) OWNER TO nexus_service;
REVOKE ALL ON FUNCTION check_and_incr_query_grace(bigint, boolean, boolean, int) FROM PUBLIC;
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_app') THEN
        GRANT EXECUTE ON FUNCTION check_and_incr_query_grace(bigint, boolean, boolean, int) TO nexus_app;
    END IF;
END $$;

CREATE OR REPLACE FUNCTION set_pages_over(p_org bigint, p_over boolean)
RETURNS timestamptz
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public
AS $$
DECLARE
    v_over timestamptz;
    v_max bigint := 0;
    v_used bigint := 0;
    v_period date := date_trunc('month', now())::date;
BEGIN
    UPDATE org_limits
       SET pages_over = p_over
     WHERE organization_id = p_org
    RETURNING quota_over_since, max_queries INTO v_over, v_max;

    IF NOT FOUND THEN
        RETURN NULL;
    END IF;

    SELECT coalesce(used, 0) INTO v_used FROM org_usage
     WHERE organization_id = p_org AND period = v_period AND metric = 'query';
    v_used := coalesce(v_used, 0);
    v_max := coalesce(v_max, 0);

    IF NOT p_over AND (v_max = 0 OR v_used < v_max) THEN
        UPDATE org_limits SET quota_over_since = NULL WHERE organization_id = p_org;
        v_over := NULL;
    END IF;

    RETURN v_over;
END;
$$;

ALTER FUNCTION set_pages_over(bigint, boolean) OWNER TO nexus_service;
REVOKE ALL ON FUNCTION set_pages_over(bigint, boolean) FROM PUBLIC;

-- Arma o relógio no instante do e-mail. Não reescreve um relógio já armado.
CREATE OR REPLACE FUNCTION arm_quota_notice(p_org bigint, p_at timestamptz)
RETURNS timestamptz
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public
AS $$
DECLARE
    v_over timestamptz;
BEGIN
    UPDATE org_limits
       SET quota_over_since = coalesce(quota_over_since, coalesce(p_at, now()))
     WHERE organization_id = p_org
    RETURNING quota_over_since INTO v_over;

    RETURN v_over;
END;
$$;

ALTER FUNCTION arm_quota_notice(bigint, timestamptz) OWNER TO nexus_service;
REVOKE ALL ON FUNCTION arm_quota_notice(bigint, timestamptz) FROM PUBLIC;
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_app') THEN
        GRANT EXECUTE ON FUNCTION set_pages_over(bigint, boolean) TO nexus_app;
        GRANT EXECUTE ON FUNCTION arm_quota_notice(bigint, timestamptz) TO nexus_app;
    END IF;
END $$;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION IF EXISTS arm_quota_notice(bigint, timestamptz);
DROP FUNCTION IF EXISTS set_pages_over(bigint, boolean);
DROP FUNCTION IF EXISTS check_and_incr_query_grace(bigint, boolean, boolean, int);
ALTER TABLE org_limits DROP COLUMN IF EXISTS pages_over;
ALTER TABLE org_limits DROP COLUMN IF EXISTS quota_over_since;
-- +goose StatementEnd
