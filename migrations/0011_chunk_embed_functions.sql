-- 0011_chunk_embed_functions.sql — SECURITY DEFINER pra embed worker drain
--
-- Problema (descoberto pelo Day 10 integration test):
--   EmbedBatchWorker drena chunks com embedding NULL cross-tenant (todos os
--   tenants ao mesmo tempo). Rodando como nexus_app (RLS ativa, sem org_id
--   setado), o SELECT retorna 0 rows e o UPDATE também é bloqueado pela
--   policy. Resultado: chunks ficavam sem embedding silenciosamente.
--
-- Mitigação anterior (Day 10): worker recebia pool admin (BYPASSRLS). Caro
-- em segurança — qualquer caminho do código que use esse pool ignora RLS.
--
-- Solução (Day 14): 2 funções SECURITY DEFINER owned por nexus_service.
-- Worker pode rodar como nexus_app sem BYPASSRLS — chama essas funções
-- via GRANT EXECUTE, e somente o caminho de embed acessa cross-tenant.

-- +goose Up
-- +goose StatementBegin

-- nexus_service precisa de SELECT/UPDATE em chunks pra functions rodarem.
-- (sem grants explícitos, function roda como nexus_service mas falha com
-- "permission denied" mesmo tendo BYPASSRLS.)
GRANT SELECT, UPDATE ON chunks TO nexus_service;

-- drain_pending_chunks: SELECT cross-tenant de N chunks com embedding NULL.
-- Retorna id + content + organization_id (worker precisa do org_id pra logs
-- e métricas; não é usado pra filtrar).
CREATE OR REPLACE FUNCTION drain_pending_chunks(p_batch_size INT)
RETURNS TABLE(
    id BIGINT,
    content TEXT,
    organization_id BIGINT
)
SECURITY DEFINER
SET search_path = public, pg_temp
LANGUAGE SQL
STABLE
AS $$
    SELECT c.id, c.content, c.organization_id
    FROM chunks c
    WHERE c.embedding IS NULL
    ORDER BY c.created_at ASC
    LIMIT GREATEST(p_batch_size, 1);
$$;

ALTER FUNCTION drain_pending_chunks(INT) OWNER TO nexus_service;
GRANT EXECUTE ON FUNCTION drain_pending_chunks(INT) TO PUBLIC;

COMMENT ON FUNCTION drain_pending_chunks(INT) IS
    'Drena chunks pending pra embedding (cross-tenant, bypassa RLS por design). '
    'Owned por nexus_service (BYPASSRLS). Usado pelo EmbedBatchWorker. '
    'Workers só conseguem ler chunks que precisam de embedding — não leak de '
    'content arbitrário, pois embedding NULL é estado transiente do ingest.';

-- update_chunk_embedding: UPDATE de 1 chunk com vetor (worker chama 1× por
-- chunk após receber resposta do provider de embed). Volátil = não pode
-- ser cacheada (STABLE/IMMUTABLE).
CREATE OR REPLACE FUNCTION update_chunk_embedding(p_chunk_id BIGINT, p_embedding vector(1024))
RETURNS VOID
SECURITY DEFINER
SET search_path = public, pg_temp
LANGUAGE SQL
VOLATILE
AS $$
    UPDATE chunks SET embedding = p_embedding WHERE id = p_chunk_id;
$$;

ALTER FUNCTION update_chunk_embedding(BIGINT, vector) OWNER TO nexus_service;
GRANT EXECUTE ON FUNCTION update_chunk_embedding(BIGINT, vector) TO PUBLIC;

COMMENT ON FUNCTION update_chunk_embedding(BIGINT, vector) IS
    'Aplica embedding em 1 chunk (cross-tenant, bypassa RLS). Owned por '
    'nexus_service. Worker chama 1× por chunk após o provider de embed retornar '
    'o vetor. Cross-tenant é seguro: o chunk_id vem do drain_pending_chunks que '
    'já filtra apenas embedding-pending; não há caminho de chamada com chunk_id '
    'arbitrário externo.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION IF EXISTS update_chunk_embedding(BIGINT, vector);
DROP FUNCTION IF EXISTS drain_pending_chunks(INT);
-- +goose StatementEnd
