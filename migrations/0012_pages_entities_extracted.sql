-- 0012_pages_entities_extracted.sql — flag de processamento de entities por page
--
-- ExtractEntitiesJob (Day 15) processa pages após embedding completar. Esta
-- coluna permite ao worker filtrar páginas ainda não processadas sem ter que
-- left-joinear com a tabela entities (que seria caro com volume).
--
-- Re-extração futura: UPDATE pages SET entities_extracted_at = NULL WHERE ...
-- e os jobs serão re-enfileirados na próxima iteração.

-- +goose Up
-- +goose StatementBegin

ALTER TABLE pages ADD COLUMN entities_extracted_at TIMESTAMPTZ;

-- Index parcial: só pages pendentes (WHERE NULL). Worker query é super seletiva
-- e o index fica pequeno (~N pages pendentes vs. N total).
CREATE INDEX pages_entities_pending_idx ON pages(created_at)
    WHERE entities_extracted_at IS NULL;

COMMENT ON COLUMN pages.entities_extracted_at IS
    'Timestamp quando ExtractEntitiesJob processou esta page com sucesso. NULL = '
    'pendente. UPDATE pra NULL re-enfileira (re-extração).';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS pages_entities_pending_idx;
ALTER TABLE pages DROP COLUMN IF EXISTS entities_extracted_at;
-- +goose StatementEnd
