-- 0037_pages_content_hash.sql — dedup de memória no add_memory (Módulo A).
--
-- content_hash = SHA-256 do conteúdo NORMALIZADO (trim + lowercase + colapsa
-- whitespace), computado no ingest. Usado pelo dedup determinístico em
-- ingest_job.go: antes de inserir uma page, se já existe page VIGENTE
-- (valid_to IS NULL) com o mesmo (organization_id, domain, content_hash), é
-- re-insert literal → NOOP (não duplica). Fecha a causa raiz da colisão de slug
-- (add_memory inseria incondicional). Inspirado no dedup por hash do mem0.
--
-- Índice parcial só nas versões vigentes, espelhando pages_domain_idx (0003) e
-- pages_project_idx (0035). NÃO-unique de propósito: a decisão (NOOP vs supersede)
-- é do código, não uma constraint que abortaria a transação do ingest.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE pages ADD COLUMN IF NOT EXISTS content_hash BYTEA;
CREATE INDEX IF NOT EXISTS pages_content_hash_idx
    ON pages(organization_id, domain, content_hash) WHERE valid_to IS NULL;
COMMENT ON COLUMN pages.content_hash IS
    'SHA-256 do conteúdo normalizado (trim+lowercase+collapse). Dedup determinístico no ingest: re-insert literal com mesmo (org,domain,content_hash) vigente → NOOP.';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS pages_content_hash_idx;
ALTER TABLE pages DROP COLUMN IF EXISTS content_hash;
-- +goose StatementEnd
