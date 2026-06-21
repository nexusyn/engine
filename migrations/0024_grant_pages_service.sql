-- 0024_grant_pages_service.sql — nexus_service precisa de SELECT em pages.
--
-- list_orgs_needing_compile (0023, SECURITY DEFINER owner nexus_service) lê pages
-- cross-org pra achar orgs com fonte nova. BYPASSRLS pula as POLICIES de RLS mas
-- NÃO concede privilégio de tabela — sem este GRANT a função dá "permission denied
-- for table pages". nexus_service já é o role privilegiado cross-org (BYPASSRLS);
-- dar-lhe SELECT em pages é consistente com esse papel.

-- +goose Up
-- +goose StatementBegin
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_service') THEN
        GRANT SELECT ON pages TO nexus_service;
    END IF;
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_service') THEN
        REVOKE SELECT ON pages FROM nexus_service;
    END IF;
END $$;
-- +goose StatementEnd
