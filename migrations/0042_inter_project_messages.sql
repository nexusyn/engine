-- 0042_inter_project_messages.sql — Mensageria desacoplada entre agentes/projetos (Fase 3).
--
-- Permite que um agente trabalhando no projeto A (ex: frontend/reachyn) envie
-- tarefas, avisos ou pedidos de contrato para o projeto B (ex: backend/engine)
-- de forma assíncrona com consumo "claim-once".

-- +goose Up
-- +goose StatementBegin

CREATE TABLE inter_project_messages (
    id                BIGSERIAL PRIMARY KEY,
    organization_id   BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    from_project      TEXT NOT NULL DEFAULT '',
    to_project        TEXT NOT NULL DEFAULT '',
    sender_agent      TEXT NOT NULL DEFAULT '',
    subject           TEXT NOT NULL DEFAULT '',
    body              TEXT NOT NULL DEFAULT '',
    status            TEXT NOT NULL DEFAULT 'unread' CHECK (status IN ('unread', 'claimed', 'resolved')),
    claimed_by_agent  TEXT,
    claimed_at        TIMESTAMPTZ,
    resolved_at       TIMESTAMPTZ,
    resolution_note   TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX inter_project_messages_inbox_idx 
    ON inter_project_messages(organization_id, to_project, created_at DESC)
    WHERE status = 'unread';

-- RLS
ALTER TABLE inter_project_messages ENABLE ROW LEVEL SECURITY;
ALTER TABLE inter_project_messages FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON inter_project_messages
    USING (organization_id = current_org_id())
    WITH CHECK (organization_id = current_org_id());

COMMENT ON TABLE inter_project_messages IS
    'Caixa de correio assíncrona entre agentes de projetos distintos (claim-once).';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP POLICY IF EXISTS tenant_isolation ON inter_project_messages;
ALTER TABLE inter_project_messages DISABLE ROW LEVEL SECURITY;
DROP TABLE IF EXISTS inter_project_messages;

-- +goose StatementEnd
