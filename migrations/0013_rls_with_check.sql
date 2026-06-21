-- 0013_rls_with_check.sql — Day 21: WITH CHECK em todas policies RLS
--
-- Backlog Week 1 fechado: as policies tenant_isolation tinham só USING
-- (filtro de SELECT/UPDATE/DELETE). Sem WITH CHECK, INSERTs cross-tenant
-- não eram bloqueados — a row entrava no DB mas ficava invisível ao próprio
-- tenant (porque USING não casava). Detectável mas não-bloqueado.
--
-- Solução: ALTER POLICY ... USING (...) WITH CHECK (organization_id =
-- current_org_id()) em cada tabela tenant. Agora INSERT cross-tenant
-- falha com `new row violates row-level security policy`.

-- +goose Up
-- +goose StatementBegin

-- Tabelas com policy tenant_isolation (migrations 0008 + 0010)
DO $$
DECLARE
    t TEXT;
    tables TEXT[] := ARRAY[
        'agents', 'api_tokens', 'pages', 'sessions', 'messages',
        'entities', 'edges', 'lessons', 'decisions', 'events',
        'usage_records', 'chunks'
    ];
BEGIN
    FOREACH t IN ARRAY tables LOOP
        EXECUTE format(
            'ALTER POLICY tenant_isolation ON %I USING (organization_id = current_org_id()) WITH CHECK (organization_id = current_org_id())',
            t
        );
    END LOOP;
END$$;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Reverte: remove WITH CHECK (volta a só USING)
DO $$
DECLARE
    t TEXT;
    tables TEXT[] := ARRAY[
        'agents', 'api_tokens', 'pages', 'sessions', 'messages',
        'entities', 'edges', 'lessons', 'decisions', 'events',
        'usage_records', 'chunks'
    ];
BEGIN
    FOREACH t IN ARRAY tables LOOP
        EXECUTE format(
            'ALTER POLICY tenant_isolation ON %I USING (organization_id = current_org_id())',
            t
        );
    END LOOP;
END$$;

-- +goose StatementEnd
