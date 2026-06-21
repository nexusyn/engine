-- 0018_create_org_with_token.sql — control-plane: criar org NOVA + token bootstrap.
--
-- POR QUE SECURITY DEFINER: organizations e api_tokens têm RLS (id/org =
-- current_org_id()). O endpoint POST /v1/admin/orgs roda como nexus_app ligado à
-- org do MASTER token (ex: org1) — não consegue inserir uma org NOVA (id != org
-- atual) nem um token pra ela. Owner = nexus_service (BYPASSRLS) bypassa RLS;
-- nexus_app só chama via EXECUTE (mesmo padrão do 0014/0015).
--
-- SEM ON CONFLICT no slug de propósito: colisão de slug NÃO pode "sequestrar" a
-- org de outro cliente (devolver token de org alheia). Slug duplicado → erro →
-- o console gera slug único (base + sufixo random) e o handler retorna 409.

-- +goose Up
-- +goose StatementBegin

GRANT INSERT, SELECT, UPDATE ON organizations TO nexus_service;
GRANT INSERT, SELECT ON api_tokens TO nexus_service;
-- USAGE nas sequences: o INSERT usa nextval() do default das PKs (senão 42501).
GRANT USAGE, SELECT ON SEQUENCE organizations_id_seq TO nexus_service;
GRANT USAGE, SELECT ON SEQUENCE api_tokens_id_seq TO nexus_service;

CREATE OR REPLACE FUNCTION create_org_with_token(
    p_slug text, p_name text, p_token_hash text, p_abilities text[]
)
RETURNS TABLE(org_id bigint, token_id bigint)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public
AS $$
DECLARE
    v_org   bigint;
    v_token bigint;
BEGIN
    INSERT INTO organizations (slug, name)
    VALUES (p_slug, p_name)
    RETURNING id INTO v_org;

    INSERT INTO api_tokens (organization_id, name, token_hash, abilities)
    VALUES (v_org, 'bootstrap', p_token_hash, p_abilities)
    RETURNING id INTO v_token;

    RETURN QUERY SELECT v_org, v_token;
END;
$$;

ALTER FUNCTION create_org_with_token(text, text, text, text[]) OWNER TO nexus_service;
REVOKE ALL ON FUNCTION create_org_with_token(text, text, text, text[]) FROM PUBLIC;

DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_app') THEN
        GRANT EXECUTE ON FUNCTION create_org_with_token(text, text, text, text[]) TO nexus_app;
    END IF;
END $$;

COMMENT ON FUNCTION create_org_with_token(text, text, text, text[]) IS
    'SECURITY DEFINER (owner nexus_service/BYPASSRLS): cria org nova + token '
    'bootstrap atômico. Control-plane (POST /v1/admin/orgs), chamado pelo console '
    'com MASTER token (ability admin). Caller = nexus_app sem elevar privilégio.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP FUNCTION IF EXISTS create_org_with_token(text, text, text, text[]);
REVOKE INSERT, SELECT, UPDATE ON organizations FROM nexus_service;
REVOKE INSERT, SELECT ON api_tokens FROM nexus_service;
REVOKE USAGE, SELECT ON SEQUENCE organizations_id_seq FROM nexus_service;
REVOKE USAGE, SELECT ON SEQUENCE api_tokens_id_seq FROM nexus_service;

-- +goose StatementEnd
