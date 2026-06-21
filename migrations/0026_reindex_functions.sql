-- 0026_reindex_functions.sql — re-index de embeddings (troca de modelo de embed).
--
-- Trocar o modelo de embed exige re-embeddar TODOS os chunks (os vetores
-- armazenados são do modelo antigo; query no modelo novo não casa). O endpoint
-- admin /v1/admin/reindex-embeddings zera os embeddings e enfileira EmbedBatchJob,
-- que re-embedda com o modelo corrente (config global).
--
-- Funções SECURITY DEFINER (owned por nexus_service, BYPASSRLS) porque o reset é
-- CROSS-TENANT e a API roda como nexus_app (RLS). Mesmo padrão da 0011.

-- +goose Up
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION reset_chunk_embeddings()
RETURNS BIGINT
SECURITY DEFINER
SET search_path = public, pg_temp
LANGUAGE plpgsql
AS $$
DECLARE n BIGINT;
BEGIN
    UPDATE chunks SET embedding = NULL WHERE embedding IS NOT NULL;
    GET DIAGNOSTICS n = ROW_COUNT;
    RETURN n;
END;
$$;
ALTER FUNCTION reset_chunk_embeddings() OWNER TO nexus_service;
GRANT EXECUTE ON FUNCTION reset_chunk_embeddings() TO PUBLIC;
COMMENT ON FUNCTION reset_chunk_embeddings() IS
    'Zera embedding de TODOS os chunks (cross-tenant, BYPASSRLS) e retorna quantos. '
    'Usado pelo re-index ao trocar o modelo de embed. Depois enfileirar EmbedBatchJob.';

CREATE OR REPLACE FUNCTION count_null_chunk_embeddings()
RETURNS BIGINT
SECURITY DEFINER
SET search_path = public, pg_temp
LANGUAGE SQL
STABLE
AS $$
    SELECT count(*) FROM chunks WHERE embedding IS NULL;
$$;
ALTER FUNCTION count_null_chunk_embeddings() OWNER TO nexus_service;
GRANT EXECUTE ON FUNCTION count_null_chunk_embeddings() TO PUBLIC;
COMMENT ON FUNCTION count_null_chunk_embeddings() IS
    'Conta chunks pendentes de embedding (cross-tenant). Progresso do re-index.';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION IF EXISTS reset_chunk_embeddings();
DROP FUNCTION IF EXISTS count_null_chunk_embeddings();
-- +goose StatementEnd
