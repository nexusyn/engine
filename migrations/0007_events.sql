-- 0007_events.sql — Events (audit log) + Usage records (metering)
--
-- Events: append-only audit log de tudo que acontece (ingest, search, query, compile, etc.).
-- Usage records: tokens consumidos por provider, custo estimado, latência.
-- Ambos são logs — não bi-temporais (não fazem sentido pra append-only).

-- +goose Up
-- +goose StatementBegin

CREATE TABLE events (
    id                BIGSERIAL PRIMARY KEY,
    organization_id   BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    agent_id          BIGINT REFERENCES agents(id) ON DELETE SET NULL,

    kind              TEXT NOT NULL,           -- page.ingested | search.performed | query.completed | compile.run | lesson.promoted | ...
    payload           JSONB NOT NULL DEFAULT '{}'::jsonb,

    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX events_org_kind_idx ON events(organization_id, kind);
CREATE INDEX events_created_idx ON events(created_at DESC);
CREATE INDEX events_payload_idx ON events USING gin(payload);

-- Usage records (tokens, $, latência por provider call)
CREATE TABLE usage_records (
    id                BIGSERIAL PRIMARY KEY,
    organization_id   BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    agent_id          BIGINT REFERENCES agents(id) ON DELETE SET NULL,

    metric            TEXT NOT NULL,           -- llm_tokens | embed_tokens | rerank_calls | bytes_stored
    provider          TEXT,                    -- gemini | anthropic | voyage | ...
    model             TEXT,                    -- gemini-2.5-flash | voyage-3.5-lite | ...
    value             BIGINT NOT NULL,         -- contagem (tokens, calls, bytes)
    cost_usd          DECIMAL(10, 6),          -- custo estimado em USD
    latency_ms        INT,

    metadata          JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX usage_org_idx ON usage_records(organization_id, created_at DESC);
CREATE INDEX usage_provider_idx ON usage_records(provider, created_at DESC) WHERE provider IS NOT NULL;
CREATE INDEX usage_metric_idx ON usage_records(metric, created_at DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS usage_records;
DROP TABLE IF EXISTS events;
-- +goose StatementEnd
