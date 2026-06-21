-- 0022_chunk_dates.sql — canal de busca por DATA (normalização determinística).
--
-- POR QUE: o FTS (plainto_tsquery 'english') não casa "01 de junho de 2026" com
-- "2026.06.01" / "01/06/2026" — formatos/idiomas viram tokens diferentes, e a
-- busca vetorial borra datas. Resultado: queries por data não recuperam (fraqueza
-- do balde temporal). Solução: coluna `dates` com tokens canônicos sem separador
-- (ex.: 'd20260601'), populada no ingest (dateutil.DateSearchTokens) e consultada
-- no retrieval como canal extra (só ACRESCENTA recall). config 'simple' → o token
-- alfanumérico fica inteiro no tsvector (sem ambiguidade de parser).
--
-- ADITIVO: coluna nova + índice; NÃO altera content/content_tsv nem a busca atual.

-- +goose Up
-- +goose StatementBegin

ALTER TABLE chunks ADD COLUMN IF NOT EXISTS dates text NOT NULL DEFAULT '';
ALTER TABLE chunks ADD COLUMN IF NOT EXISTS dates_tsv tsvector
    GENERATED ALWAYS AS (to_tsvector('simple', dates)) STORED;
CREATE INDEX IF NOT EXISTS chunks_dates_tsv_idx ON chunks USING gin (dates_tsv);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS chunks_dates_tsv_idx;
ALTER TABLE chunks DROP COLUMN IF EXISTS dates_tsv;
ALTER TABLE chunks DROP COLUMN IF EXISTS dates;
-- +goose StatementEnd
