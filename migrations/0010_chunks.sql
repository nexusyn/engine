-- 0010_chunks.sql — Chunks (unidade de embedding/retrieval)
--
-- Filosofia: pages é a unidade lógica (1 source = 1 page), chunks é a unidade
-- semântica (split em janelas pra granularidade fina no retrieval).
--
-- Search opera em chunks (recall preciso). Quando precisar do contexto inteiro,
-- agrupa por page_id e devolve a página completa.

-- +goose Up
-- +goose StatementBegin

CREATE TABLE chunks (
    id              BIGSERIAL PRIMARY KEY,
    organization_id BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    page_id         BIGINT NOT NULL REFERENCES pages(id) ON DELETE CASCADE,

    position        INT NOT NULL,            -- ordem do chunk dentro da page (0-indexed)
    content         TEXT NOT NULL,
    embedding       vector(1024),
    metadata        JSONB NOT NULL DEFAULT '{}'::jsonb,

    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    UNIQUE (page_id, position)
);

CREATE INDEX chunks_org_idx ON chunks(organization_id);
CREATE INDEX chunks_page_idx ON chunks(page_id);

-- FTS pt-BR no conteúdo dos chunks (search híbrido)
ALTER TABLE chunks ADD COLUMN content_tsv tsvector
    GENERATED ALWAYS AS (
        to_tsvector('portuguese', coalesce(content, ''))
    ) STORED;
CREATE INDEX chunks_fts_idx ON chunks USING gin(content_tsv);

-- Vector index condicional (DiskANN se vectorscale disponível, HNSW se não)
DO $$
DECLARE has_vectorscale boolean;
BEGIN
    SELECT EXISTS(SELECT 1 FROM pg_available_extensions WHERE name = 'vectorscale') INTO has_vectorscale;
    IF has_vectorscale THEN
        EXECUTE 'CREATE INDEX chunks_embedding_diskann ON chunks
                 USING diskann (embedding vector_cosine_ops)
                 WITH (storage_layout = ''memory_optimized'', num_neighbors = 50, num_dimensions = 1024, num_bits_per_dimension = 1)';
    ELSE
        EXECUTE 'CREATE INDEX chunks_embedding_hnsw ON chunks
                 USING hnsw (embedding vector_cosine_ops) WITH (m=16, ef_construction=64)';
    END IF;
END$$;

-- RLS
ALTER TABLE chunks ENABLE ROW LEVEL SECURITY;
ALTER TABLE chunks FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON chunks
    USING (organization_id = current_org_id());

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS chunks;
-- +goose StatementEnd
