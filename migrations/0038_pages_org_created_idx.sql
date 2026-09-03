-- 0038_pages_org_created_idx.sql — índice composto (organization_id, created_at DESC) em pages
--
-- Achado de desempenho: pages_org_current_idx (0003_pages.sql) é só igualdade
-- em organization_id — não cobre ORDER BY created_at (listagem "memórias
-- recentes da org"), que cai em sort sem índice. Outras tabelas já seguem o
-- padrão correto: usage_records tem (organization_id, created_at DESC)
-- (0007_events.sql, usage_org_idx). Este índice replica o mesmo padrão pra
-- pages, restrito às versões vigentes (valid_to IS NULL), como os demais
-- índices parciais de pages.
--
-- CONCURRENTLY pra não travar a tabela em produção (pages pode ser grande e
-- é a tabela mais quente do engine). Isso exige a diretiva NO TRANSACTION do
-- goose (ver o bloco de anotações no fim do arquivo): Postgres rejeita
-- CREATE INDEX CONCURRENTLY dentro de bloco de transação, e
-- o goose (pressly/goose v3.27.1, ver cmd/nexus/migrate.go) roda cada
-- migration dentro de uma transação por padrão. Nenhuma outra migration
-- deste repo usa CONCURRENTLY ainda — todas as demais criam índice normal
-- dentro da transação padrão do goose (ex.: 0027_org_usage.sql,
-- 0034_admin_usage_overview.sql). Esta é a primeira que precisa do modo sem
-- transação, dado o volume de linhas de pages em produção.
--
-- ⚠️ Este arquivo só cria a migration — NÃO foi aplicada contra nenhum banco
-- (nem local, nem produção). Rodar migration é ação persistente (N2) e
-- precisa de autorização explícita antes de aplicar.

-- +goose NO TRANSACTION
-- +goose Up
CREATE INDEX CONCURRENTLY IF NOT EXISTS pages_org_created_idx
    ON pages(organization_id, created_at DESC)
    WHERE valid_to IS NULL;

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS pages_org_created_idx;
