-- 0005_entities_graph.sql — Entities + Edges (knowledge graph bi-temporal)
--
-- Entities = pessoas, datas, lugares, conceitos, eventos extraídos do conteúdo.
-- Edges = relações ENTRE entidades, com timestamps de validade (estilo Graphiti).
--
-- Multi-hop é feito via CTE recursiva em edges (semana 4 do roadmap).
-- Bi-temporal: edges podem ser invalidadas sem deletadas, preservando história.

-- +goose Up
-- +goose StatementBegin

CREATE TABLE entities (
    id                BIGSERIAL PRIMARY KEY,
    organization_id   BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,

    slug              TEXT NOT NULL,
    kind              TEXT NOT NULL,           -- person | date | place | concept | event | organization | ...
    name              TEXT NOT NULL,
    aliases           TEXT[] NOT NULL DEFAULT '{}',
    attributes        JSONB NOT NULL DEFAULT '{}'::jsonb,  -- ex: {age: 32, birthday: "1990-04-15"} pra person

    embedding         vector(1024),

    -- Bi-temporal
    valid_from        TIMESTAMPTZ NOT NULL DEFAULT now(),
    valid_to          TIMESTAMPTZ,
    transaction_time  TIMESTAMPTZ NOT NULL DEFAULT now(),

    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

    UNIQUE (organization_id, slug)
);

CREATE INDEX entities_kind_idx ON entities(organization_id, kind) WHERE valid_to IS NULL;
CREATE INDEX entities_name_trgm_idx ON entities USING gin(name gin_trgm_ops);
CREATE INDEX entities_aliases_idx ON entities USING gin(aliases);

-- Vector index pra busca semântica em entities
DO $$
DECLARE has_vectorscale boolean;
BEGIN
    SELECT EXISTS(SELECT 1 FROM pg_available_extensions WHERE name = 'vectorscale') INTO has_vectorscale;
    IF has_vectorscale THEN
        EXECUTE 'CREATE INDEX entities_embedding_diskann ON entities
                 USING diskann (embedding vector_cosine_ops)
                 WITH (storage_layout = ''memory_optimized'', num_neighbors = 50, num_dimensions = 1024, num_bits_per_dimension = 1)';
    ELSE
        EXECUTE 'CREATE INDEX entities_embedding_hnsw ON entities
                 USING hnsw (embedding vector_cosine_ops) WITH (m=16, ef_construction=64)';
    END IF;
END$$;

-- Edges (relações bi-temporais)
CREATE TABLE edges (
    id                BIGSERIAL PRIMARY KEY,
    organization_id   BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,

    from_entity_id    BIGINT NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    to_entity_id      BIGINT NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    kind              TEXT NOT NULL,           -- mentions | caused | happened_at | located_in | works_for | ...
    weight            REAL NOT NULL DEFAULT 1.0,

    source_page_id    BIGINT REFERENCES pages(id) ON DELETE SET NULL,  -- onde a relação foi extraída
    metadata          JSONB NOT NULL DEFAULT '{}'::jsonb,

    -- Bi-temporal
    valid_from        TIMESTAMPTZ NOT NULL DEFAULT now(),
    valid_to          TIMESTAMPTZ,
    transaction_time  TIMESTAMPTZ NOT NULL DEFAULT now(),

    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Multi-hop queries usam estes índices (CTE recursiva fan-out a partir de from_entity_id)
CREATE INDEX edges_from_idx ON edges(from_entity_id, kind) WHERE valid_to IS NULL;
CREATE INDEX edges_to_idx ON edges(to_entity_id, kind) WHERE valid_to IS NULL;
CREATE INDEX edges_org_idx ON edges(organization_id);
CREATE INDEX edges_source_idx ON edges(source_page_id) WHERE source_page_id IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS edges;
DROP TABLE IF EXISTS entities;
-- +goose StatementEnd
