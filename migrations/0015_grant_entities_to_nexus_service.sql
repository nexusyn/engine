-- 0015_grant_entities_to_nexus_service.sql — GRANT SELECT on entities to
-- nexus_service so SECURITY DEFINER helpers owned by it (e.g.
-- list_orgs_with_profile_facts from migration 0014) can read across tenants.
--
-- Without this GRANT, the function fails at runtime with:
--   "permission denied for table entities (SQLSTATE 42501)"
-- Discovered during Sprint 3.1 first run.
--
-- Pattern: anytime a future SECURITY DEFINER helper needs cross-tenant
-- visibility into table X, add GRANT SELECT ON X TO nexus_service here
-- (or in a new migration).

-- +goose Up
-- +goose StatementBegin

GRANT SELECT ON entities TO nexus_service;

COMMENT ON ROLE nexus_service IS
    'BYPASSRLS NOLOGIN role used as owner of SECURITY DEFINER helpers that '
    'need cross-tenant visibility. App connects as nexus_app and calls these '
    'helpers without elevating its own privileges. Grant SELECT on additional '
    'tables here as new helpers are added.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

REVOKE SELECT ON entities FROM nexus_service;

-- +goose StatementEnd
