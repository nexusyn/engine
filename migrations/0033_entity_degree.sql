-- 0033_entity_degree.sql — Fase 1 GraphRAG: grau pré-computado pro hub-damping.
-- A graph-expansion varria as ~29k edges a CADA query (CTE hubs com UNION+GROUP BY,
-- ~130ms) só pra achar os super-hubs. Materializa o grau (nº de edges current) numa
-- coluna; o hub-damping vira um lookup por PK no walk. Mantida atualizada no persist
-- (extract) — ver internal/core/entities/persist.go.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE entities ADD COLUMN IF NOT EXISTS degree INTEGER NOT NULL DEFAULT 0;

-- Backfill global (todas as orgs): grau = nº de edges current que tocam a entity.
UPDATE entities e SET degree = COALESCE(d.c, 0)
FROM (
    SELECT eid, count(*) AS c FROM (
        SELECT from_entity_id AS eid FROM edges WHERE valid_to IS NULL
        UNION ALL SELECT to_entity_id FROM edges WHERE valid_to IS NULL
    ) z GROUP BY eid
) d
WHERE e.id = d.eid;

COMMENT ON COLUMN entities.degree IS
    'Grau current (nº de edges com valid_to IS NULL que tocam a entity). Hub-damping da graph-expansion (Fase 1). Mantido no persist após cada extração.';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE entities DROP COLUMN IF EXISTS degree;
-- +goose StatementEnd
