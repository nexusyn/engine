-- 0017_webhooks.sql — Config de webhooks por organização (tela Webhooks do
-- dashboard). v1 = CRUD de endpoints (url + evento + ativo). A ENTREGA (chamar
-- o webhook quando eventos acontecem) é um increment futuro; por ora a tabela
-- guarda a configuração e o dashboard gerencia.

-- +goose Up
-- +goose StatementBegin

CREATE TABLE webhooks (
    id              BIGSERIAL PRIMARY KEY,
    organization_id BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    url             TEXT NOT NULL,
    event           TEXT NOT NULL DEFAULT '*',
    active          BOOLEAN NOT NULL DEFAULT true,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX webhooks_org_idx ON webhooks(organization_id);

ALTER TABLE webhooks ENABLE ROW LEVEL SECURITY;
ALTER TABLE webhooks FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON webhooks
    USING (organization_id = current_org_id())
    WITH CHECK (organization_id = current_org_id());

COMMENT ON TABLE webhooks IS
    'Config de webhooks por org (url/event/active). Entrega de eventos = futuro.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP POLICY IF EXISTS tenant_isolation ON webhooks;
ALTER TABLE webhooks DISABLE ROW LEVEL SECURITY;
DROP TABLE IF EXISTS webhooks;

-- +goose StatementEnd
