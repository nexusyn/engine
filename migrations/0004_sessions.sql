-- 0004_sessions.sql — Sessions + Messages (memória episódica)
--
-- Sessions = conversações. Messages = turns individuais.
-- Cada session pode ter um summary (gerado por LLM ao "compactar") + embedding pra recall.

-- +goose Up
-- +goose StatementBegin

CREATE TABLE sessions (
    id                BIGSERIAL PRIMARY KEY,
    organization_id   BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    agent_id          BIGINT REFERENCES agents(id) ON DELETE SET NULL,

    external_id       TEXT,                    -- ID externo do cliente (ex: claude-session-XYZ)
    summary           TEXT,
    embedding         vector(1024),

    metadata          JSONB NOT NULL DEFAULT '{}'::jsonb,

    started_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    ended_at          TIMESTAMPTZ,
    compacted_at      TIMESTAMPTZ,             -- quando o summary foi gerado

    valid_from        TIMESTAMPTZ NOT NULL DEFAULT now(),
    valid_to          TIMESTAMPTZ,
    transaction_time  TIMESTAMPTZ NOT NULL DEFAULT now(),

    UNIQUE (organization_id, external_id)
);

CREATE INDEX sessions_org_idx ON sessions(organization_id) WHERE valid_to IS NULL;
CREATE INDEX sessions_agent_idx ON sessions(agent_id) WHERE agent_id IS NOT NULL;
CREATE INDEX sessions_started_idx ON sessions(started_at DESC);

-- Vector index pra recall semântico em sessions (busca "sessões parecidas com X")
DO $$
DECLARE has_vectorscale boolean;
BEGIN
    SELECT EXISTS(SELECT 1 FROM pg_available_extensions WHERE name = 'vectorscale') INTO has_vectorscale;
    IF has_vectorscale THEN
        EXECUTE 'CREATE INDEX sessions_embedding_diskann ON sessions
                 USING diskann (embedding vector_cosine_ops)
                 WITH (storage_layout = ''memory_optimized'', num_neighbors = 50, num_dimensions = 1024, num_bits_per_dimension = 1)';
    ELSE
        EXECUTE 'CREATE INDEX sessions_embedding_hnsw ON sessions
                 USING hnsw (embedding vector_cosine_ops) WITH (m=16, ef_construction=64)';
    END IF;
END$$;

CREATE TABLE messages (
    id                BIGSERIAL PRIMARY KEY,
    organization_id   BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    session_id        BIGINT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,

    role              TEXT NOT NULL CHECK (role IN ('user', 'assistant', 'system', 'tool')),
    content           TEXT NOT NULL,
    metadata          JSONB NOT NULL DEFAULT '{}'::jsonb,

    -- Sequência dentro da session
    seq               INT NOT NULL,

    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

    UNIQUE (session_id, seq)
);

CREATE INDEX messages_session_idx ON messages(session_id, seq);
CREATE INDEX messages_org_idx ON messages(organization_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS messages;
DROP TABLE IF EXISTS sessions;
-- +goose StatementEnd
