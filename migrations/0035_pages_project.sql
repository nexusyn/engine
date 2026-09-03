-- 0035: coluna `project` em pages — memória por projeto (Design A).
--
-- Segmenta memórias por PROJETO dentro da org (ex: nexusyn/reachyn/foxinfluencer),
-- espelhando o mecanismo de `domain`. project NULL = global/geral.
--
-- Backward-compatible: memórias existentes ficam project=NULL e seguem aparecendo
-- em TODA busca — a busca filtrada usa `project = $N OR project IS NULL` (globais
-- sempre incluídas, decisão de produto F0/2026-06-24).
--
-- É FILTRO DE APLICAÇÃO dentro do tenant, NÃO isolamento RLS. O isolamento
-- cross-tenant (organization_id, FORCE RLS) permanece o único eixo de segurança.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE pages ADD COLUMN project TEXT;

-- Espelha pages_domain_idx (0003): partial index só nas versões vigentes (valid_to IS NULL).
CREATE INDEX pages_project_idx ON pages(organization_id, project) WHERE valid_to IS NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS pages_project_idx;
ALTER TABLE pages DROP COLUMN IF EXISTS project;
-- +goose StatementEnd
