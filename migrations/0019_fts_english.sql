-- 0019_fts_english.sql — FTS: dicionário 'portuguese' → 'english'
--
-- Motivo: o produto e o benchmark (LongMemEval) são em inglês, mas as colunas
-- geradas content_tsv (0003 pages, 0010 chunks) e a query (queries.go) usavam
-- stemming PORTUGUÊS sobre texto inglês — degrada o casamento de palavra-chave
-- ("attend"/"attending" não normalizavam). Reconstrói os tsvector em 'english'.
--
-- Generated column não permite ALTER da expressão → DROP + ADD (recomputa todas
-- as linhas) + recria o índice GIN. queries.go usa o mesmo 'english' (ftsConfig).
-- TODO futuro: idioma por-org (coluna lang + tsvector via trigger) p/ corpora PT.

-- +goose Up
ALTER TABLE pages DROP COLUMN IF EXISTS content_tsv;
ALTER TABLE pages ADD COLUMN content_tsv tsvector
    GENERATED ALWAYS AS (
        to_tsvector('english', coalesce(title, '') || ' ' || coalesce(content, ''))
    ) STORED;
CREATE INDEX IF NOT EXISTS pages_fts_idx ON pages USING gin(content_tsv);

ALTER TABLE chunks DROP COLUMN IF EXISTS content_tsv;
ALTER TABLE chunks ADD COLUMN content_tsv tsvector
    GENERATED ALWAYS AS (
        to_tsvector('english', coalesce(content, ''))
    ) STORED;
CREATE INDEX IF NOT EXISTS chunks_fts_idx ON chunks USING gin(content_tsv);

-- +goose Down
ALTER TABLE pages DROP COLUMN IF EXISTS content_tsv;
ALTER TABLE pages ADD COLUMN content_tsv tsvector
    GENERATED ALWAYS AS (
        to_tsvector('portuguese', coalesce(title, '') || ' ' || coalesce(content, ''))
    ) STORED;
CREATE INDEX IF NOT EXISTS pages_fts_idx ON pages USING gin(content_tsv);

ALTER TABLE chunks DROP COLUMN IF EXISTS content_tsv;
ALTER TABLE chunks ADD COLUMN content_tsv tsvector
    GENERATED ALWAYS AS (
        to_tsvector('portuguese', coalesce(content, ''))
    ) STORED;
CREATE INDEX IF NOT EXISTS chunks_fts_idx ON chunks USING gin(content_tsv);
