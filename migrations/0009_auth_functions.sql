-- 0009_auth_functions.sql — SECURITY DEFINER pra auth bypass RLS
--
-- Problema: auth middleware precisa consultar api_tokens ANTES de saber o org_id.
-- Mas RLS de api_tokens exige org_id setado → query retorna zero rows sempre.
--
-- Solução: função SECURITY DEFINER. Owned por nexus_service (BYPASSRLS), retorna
-- o token sem RLS aplicado. Aplicação chama essa função com o hash; aprovação
-- depende apenas do hash bater (que é alta entropia, não-adivinhável).

-- +goose Up
-- +goose StatementBegin

-- nexus_service precisa de SELECT em api_tokens pra função SECURITY DEFINER rodar.
-- (sem isso, função roda como nexus_service mas falha com "permission denied"
-- mesmo tendo BYPASSRLS — bypass de RLS não dá grant.)
GRANT SELECT ON api_tokens TO nexus_service;

CREATE OR REPLACE FUNCTION find_api_token_by_hash(p_hash TEXT)
RETURNS TABLE(
    id BIGINT,
    organization_id BIGINT,
    abilities TEXT[],
    expires_at TIMESTAMPTZ
)
SECURITY DEFINER
SET search_path = public, pg_temp
LANGUAGE SQL
STABLE
AS $$
    SELECT id, organization_id, abilities, expires_at
    FROM api_tokens
    WHERE token_hash = p_hash
      AND (expires_at IS NULL OR expires_at > now())
    LIMIT 1;
$$;

-- Owner = nexus_service (já tem BYPASSRLS pela migration 0008)
ALTER FUNCTION find_api_token_by_hash(TEXT) OWNER TO nexus_service;

-- Pode ser chamada por qualquer role autenticada (public role no Postgres)
GRANT EXECUTE ON FUNCTION find_api_token_by_hash(TEXT) TO PUBLIC;

COMMENT ON FUNCTION find_api_token_by_hash(TEXT) IS
    'Lookup de api_token via hash, bypassa RLS por design (auth flow). '
    'Owned por nexus_service (BYPASSRLS). Seguro porque hash é alta-entropia '
    '(SHA-256 de 256 bits random) — adivinhar é computacionalmente inviável.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION IF EXISTS find_api_token_by_hash(TEXT);
-- +goose StatementEnd
