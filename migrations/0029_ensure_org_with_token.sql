-- 0029_ensure_org_with_token.sql — variante IDEMPOTENTE de create_org_with_token,
-- só para o CLI `nexus admin create-token` (self-host / operador).
--
-- Diferença da 0018: a 0018 FALHA em slug duplicado DE PROPÓSITO — no caminho
-- multi-tenant (console / POST /v1/admin/orgs) uma colisão de slug não pode
-- devolver token de org alheia. Já o CLI roda no servidor do próprio operador,
-- onde re-rodar o comando (script, retry, esqueci o token) NÃO deve quebrar.
--
-- Esta função: reusa a org se o slug já existe (find-or-create), e SEMPRE emite
-- um token novo (tokens não são recuperáveis — o antigo segue válido, revogável).
-- Retorna org_created pra o CLI avisar se criou ou reusou. SECURITY DEFINER
-- (owner nexus_service) pelos mesmos motivos da 0018 (RLS em organizations/api_tokens).

-- +goose Up
-- +goose StatementBegin

CREATE OR REPLACE FUNCTION ensure_org_with_token(
    p_slug text, p_name text, p_token_hash text, p_abilities text[]
)
RETURNS TABLE(org_id bigint, token_id bigint, org_created boolean)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public
AS $$
DECLARE
    v_org     bigint;
    v_token   bigint;
    v_created boolean := false;
BEGIN
    SELECT id INTO v_org FROM organizations WHERE slug = p_slug;
    IF v_org IS NULL THEN
        INSERT INTO organizations (slug, name) VALUES (p_slug, p_name)
        RETURNING id INTO v_org;
        v_created := true;
    END IF;

    INSERT INTO api_tokens (organization_id, name, token_hash, abilities)
    VALUES (v_org, 'bootstrap', p_token_hash, p_abilities)
    RETURNING id INTO v_token;

    RETURN QUERY SELECT v_org, v_token, v_created;
END;
$$;

ALTER FUNCTION ensure_org_with_token(text, text, text, text[]) OWNER TO nexus_service;
REVOKE ALL ON FUNCTION ensure_org_with_token(text, text, text, text[]) FROM PUBLIC;
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_app') THEN
        GRANT EXECUTE ON FUNCTION ensure_org_with_token(text, text, text, text[]) TO nexus_app;
    END IF;
END $$;

COMMENT ON FUNCTION ensure_org_with_token(text, text, text, text[]) IS
    'SECURITY DEFINER (owner nexus_service/BYPASSRLS): find-or-create da org pelo '
    'slug + SEMPRE emite token novo. Idempotente quanto à org — só para o CLI '
    'self-host (nexus admin create-token). NÃO usar no caminho multi-tenant do '
    'console (use create_org_with_token, que falha em slug duplicado de propósito).';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION IF EXISTS ensure_org_with_token(text, text, text, text[]);
-- +goose StatementEnd
