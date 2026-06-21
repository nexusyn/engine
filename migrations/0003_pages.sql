-- 0003_pages.sql — Pages (memória semântica) com bi-temporal + embedding + FTS pt-BR
--
-- Pages é a tabela central — guarda wiki, sources, compiled knowledge, lessons (via referência).
-- Bi-temporal: valid_from/valid_to permitem responder "qual era o estado em X data?"
-- Supersede: nova versão seta valid_to da anterior, em vez de deletar.
--
-- Índice vetorial: tenta DiskANN (pgvectorscale) se disponível, fallback HNSW (pgvector puro).
-- Permite que CI (Postgres alpine sem vectorscale) use HNSW; prod (timescaledb-ha) usa DiskANN.

-- +goose Up
-- +goose StatementBegin

-- Extensões necessárias
CREATE EXTENSION IF NOT EXISTS vector;        -- pgvector (sempre disponível)
CREATE EXTENSION IF NOT EXISTS pg_trgm;       -- trigram pra fuzzy match em title
CREATE EXTENSION IF NOT EXISTS unaccent;      -- normaliza acentos pra FTS pt-BR

-- Tabela principal
CREATE TABLE pages (
    id                BIGSERIAL PRIMARY KEY,
    organization_id   BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    agent_id          BIGINT REFERENCES agents(id) ON DELETE SET NULL,

    slug              TEXT NOT NULL,
    title             TEXT NOT NULL,
    domain            TEXT NOT NULL DEFAULT 'memory',     -- memory | wiki | source | lesson | decision
    content           TEXT NOT NULL,

    embedding         vector(1024),                        -- Voyage 3.5-lite dimensions
    source_type       TEXT,                                -- raw | compiled
    metadata          JSONB NOT NULL DEFAULT '{}'::jsonb,

    -- Bi-temporal (Graphiti-inspired)
    valid_from        TIMESTAMPTZ NOT NULL DEFAULT now(),
    valid_to          TIMESTAMPTZ,                         -- NULL = atual; setado quando supersede
    transaction_time  TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Versioning chain
    superseded_by     BIGINT REFERENCES pages(id),

    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Slug único por org E versão temporal
    UNIQUE (organization_id, slug, transaction_time)
);

-- Índices base
CREATE INDEX pages_org_current_idx ON pages(organization_id) WHERE valid_to IS NULL;
CREATE INDEX pages_domain_idx ON pages(organization_id, domain) WHERE valid_to IS NULL;
CREATE INDEX pages_agent_idx ON pages(agent_id) WHERE agent_id IS NOT NULL;
CREATE INDEX pages_metadata_idx ON pages USING gin(metadata);

-- FTS português via generated column STORED.
-- NOTA: Postgres requer expressão IMMUTABLE pra generated column. unaccent() é
-- STABLE por default (depende de dicionário). Aplicamos unaccent na QUERY time
-- (via plainto_tsquery wrapper) em vez de aqui. Stemming pt-BR já é forte sem ele.
ALTER TABLE pages ADD COLUMN content_tsv tsvector
    GENERATED ALWAYS AS (
        to_tsvector('portuguese', coalesce(title, '') || ' ' || coalesce(content, ''))
    ) STORED;

CREATE INDEX pages_fts_idx ON pages USING gin(content_tsv);

-- Trigram em title pra autocomplete / fuzzy search
CREATE INDEX pages_title_trgm_idx ON pages USING gin(title gin_trgm_ops);

-- Vector index: tenta DiskANN, fallback HNSW
DO $$
DECLARE
    has_vectorscale boolean;
BEGIN
    SELECT EXISTS(SELECT 1 FROM pg_available_extensions WHERE name = 'vectorscale') INTO has_vectorscale;

    IF has_vectorscale THEN
        EXECUTE 'CREATE EXTENSION IF NOT EXISTS vectorscale CASCADE';
        EXECUTE 'CREATE INDEX pages_embedding_diskann ON pages
                 USING diskann (embedding vector_cosine_ops)
                 WITH (storage_layout = ''memory_optimized'',
                       num_neighbors = 50,
                       search_list_size = 100,
                       max_alpha = 1.2,
                       num_dimensions = 1024,
                       num_bits_per_dimension = 1)';
        RAISE NOTICE 'pages: DiskANN+SBQ index created (pgvectorscale)';
    ELSE
        EXECUTE 'CREATE INDEX pages_embedding_hnsw ON pages
                 USING hnsw (embedding vector_cosine_ops)
                 WITH (m=16, ef_construction=64)';
        RAISE NOTICE 'pages: HNSW index created (pgvector only — pgvectorscale not available)';
    END IF;
END$$;

-- Updated_at trigger (reusa função do 0001)
CREATE OR REPLACE FUNCTION pages_updated_at_trigger()
RETURNS TRIGGER AS $$
BEGIN
    NEW.transaction_time = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER pages_set_transaction_time
    BEFORE UPDATE ON pages
    FOR EACH ROW EXECUTE FUNCTION pages_updated_at_trigger();

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS pages_set_transaction_time ON pages;
DROP FUNCTION IF EXISTS pages_updated_at_trigger();
DROP TABLE IF EXISTS pages;
-- Extensões mantidas (podem ser usadas por outras tabelas)
-- +goose StatementEnd
