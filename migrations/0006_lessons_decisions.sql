-- 0006_lessons_decisions.sql — Lessons (twice-rule) + Decisions
--
-- Lessons: aprendizados promovidos via twice-rule (registra 2× → promoted=true).
-- Decisions: decisões arquiteturais com rationale.
-- Ambas são "first-class" — query patterns diferentes de pages genéricos.

-- +goose Up
-- +goose StatementBegin

CREATE TABLE lessons (
    id                BIGSERIAL PRIMARY KEY,
    organization_id   BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    agent_id          BIGINT REFERENCES agents(id) ON DELETE SET NULL,

    slug              TEXT NOT NULL,
    title             TEXT NOT NULL,
    body              TEXT NOT NULL,
    severity          TEXT NOT NULL DEFAULT 'medium' CHECK (severity IN ('critical', 'high', 'medium', 'low')),

    -- Twice-rule
    occurrence_count  INT NOT NULL DEFAULT 1,
    promoted          BOOLEAN NOT NULL DEFAULT false,    -- vira true em 2ª ocorrência
    promoted_at       TIMESTAMPTZ,
    prevention        TEXT,                              -- como evitar no futuro (gerado quando promotes)

    tags              TEXT[] NOT NULL DEFAULT '{}',
    embedding         vector(1024),
    metadata          JSONB NOT NULL DEFAULT '{}'::jsonb,

    valid_from        TIMESTAMPTZ NOT NULL DEFAULT now(),
    valid_to          TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

    UNIQUE (organization_id, slug)
);

CREATE INDEX lessons_org_idx ON lessons(organization_id) WHERE valid_to IS NULL;
CREATE INDEX lessons_promoted_idx ON lessons(organization_id, promoted) WHERE valid_to IS NULL;
CREATE INDEX lessons_severity_idx ON lessons(severity);
CREATE INDEX lessons_tags_idx ON lessons USING gin(tags);

-- Vector index
DO $$
DECLARE has_vectorscale boolean;
BEGIN
    SELECT EXISTS(SELECT 1 FROM pg_available_extensions WHERE name = 'vectorscale') INTO has_vectorscale;
    IF has_vectorscale THEN
        EXECUTE 'CREATE INDEX lessons_embedding_diskann ON lessons
                 USING diskann (embedding vector_cosine_ops)
                 WITH (storage_layout = ''memory_optimized'', num_neighbors = 50, num_dimensions = 1024, num_bits_per_dimension = 1)';
    ELSE
        EXECUTE 'CREATE INDEX lessons_embedding_hnsw ON lessons
                 USING hnsw (embedding vector_cosine_ops) WITH (m=16, ef_construction=64)';
    END IF;
END$$;

CREATE TRIGGER lessons_set_updated_at
    BEFORE UPDATE ON lessons
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ───── Decisions ─────

CREATE TABLE decisions (
    id                BIGSERIAL PRIMARY KEY,
    organization_id   BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    agent_id          BIGINT REFERENCES agents(id) ON DELETE SET NULL,

    slug              TEXT NOT NULL,
    title             TEXT NOT NULL,
    description       TEXT NOT NULL,
    category          TEXT,                              -- architecture | product | ops | ...
    rationale         TEXT,                              -- por que decidiu
    consequences      TEXT,                              -- o que isso implica
    status            TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'superseded', 'reverted')),

    tags              TEXT[] NOT NULL DEFAULT '{}',
    embedding         vector(1024),
    metadata          JSONB NOT NULL DEFAULT '{}'::jsonb,

    superseded_by     BIGINT REFERENCES decisions(id),

    valid_from        TIMESTAMPTZ NOT NULL DEFAULT now(),
    valid_to          TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

    UNIQUE (organization_id, slug)
);

CREATE INDEX decisions_org_idx ON decisions(organization_id) WHERE valid_to IS NULL;
CREATE INDEX decisions_status_idx ON decisions(status);
CREATE INDEX decisions_category_idx ON decisions(category) WHERE category IS NOT NULL;
CREATE INDEX decisions_tags_idx ON decisions USING gin(tags);

DO $$
DECLARE has_vectorscale boolean;
BEGIN
    SELECT EXISTS(SELECT 1 FROM pg_available_extensions WHERE name = 'vectorscale') INTO has_vectorscale;
    IF has_vectorscale THEN
        EXECUTE 'CREATE INDEX decisions_embedding_diskann ON decisions
                 USING diskann (embedding vector_cosine_ops)
                 WITH (storage_layout = ''memory_optimized'', num_neighbors = 50, num_dimensions = 1024, num_bits_per_dimension = 1)';
    ELSE
        EXECUTE 'CREATE INDEX decisions_embedding_hnsw ON decisions
                 USING hnsw (embedding vector_cosine_ops) WITH (m=16, ef_construction=64)';
    END IF;
END$$;

CREATE TRIGGER decisions_set_updated_at
    BEFORE UPDATE ON decisions
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS decisions_set_updated_at ON decisions;
DROP TRIGGER IF EXISTS lessons_set_updated_at ON lessons;
DROP TABLE IF EXISTS decisions;
DROP TABLE IF EXISTS lessons;
-- +goose StatementEnd
