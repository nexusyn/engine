-- 0041_session_handoffs.sql — Passagem de bastão entre agentes (SessionEnd -> SessionStart).
--
-- Permite que um agente (ex: Claude Code) salve um snapshot compacto ao terminar
-- ou compactar uma sessão, para que o próximo agente (ex: Cursor, Antigravity)
-- receba automaticamente o estado exato e continue o trabalho sem perda de contexto.

-- +goose Up
-- +goose StatementBegin

CREATE TABLE session_handoffs (
    id                BIGSERIAL PRIMARY KEY,
    organization_id   BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    project           TEXT NOT NULL DEFAULT '',
    agent_source      TEXT NOT NULL DEFAULT '',
    branch            TEXT NOT NULL DEFAULT '',
    task_summary      TEXT NOT NULL DEFAULT '',
    active_files      TEXT[] NOT NULL DEFAULT '{}',
    next_steps        TEXT NOT NULL DEFAULT '',
    expires_at        TIMESTAMPTZ NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX session_handoffs_lookup_idx 
    ON session_handoffs(organization_id, project, expires_at, created_at DESC);

-- RLS
ALTER TABLE session_handoffs ENABLE ROW LEVEL SECURITY;
ALTER TABLE session_handoffs FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON session_handoffs
    USING (organization_id = current_org_id())
    WITH CHECK (organization_id = current_org_id());

COMMENT ON TABLE session_handoffs IS
    'Snapshots compactos de passagem de bastão entre agentes (SessionEnd -> SessionStart). TTL efêmero.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP POLICY IF EXISTS tenant_isolation ON session_handoffs;
ALTER TABLE session_handoffs DISABLE ROW LEVEL SECURITY;
DROP TABLE IF EXISTS session_handoffs;

-- +goose StatementEnd
