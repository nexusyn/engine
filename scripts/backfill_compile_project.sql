-- backfill_compile_project.sql — atribui `project` aos DERIVADOS já compilados.
--
-- Contexto: antes de fix/compile-project-scoping (commit f7252eb), o compile
-- gravava wiki/lesson/decision/error sempre com project=NULL (global). Como a busca
-- filtrada usa `project = $N OR project IS NULL`, o conhecimento DESTILADO vazava
-- entre projetos. O código já foi corrigido para o FUTURO (o compile agrupa por
-- project e propaga a proveniência); este script corrige o dado ANTIGO.
--
-- REGRA (conservadora): um derivado recebe o project P se, e só se, TODAS as suas
-- memórias-fonte (metadata.source_page_ids) têm project = P — um único project
-- não-nulo, sem NENHUMA fonte global misturada. Qualquer mistura (2+ projects, ou
-- project + global) PERMANECE global: é genuinamente cross-project e o filtro
-- sem-projeto continua trazendo-a. Não há chute.
--
-- SEGURO POR CONSTRUÇÃO:
--   * `project` NÃO faz parte da unique (organization_id, domain, slug,
--     transaction_time) → reatribuir project nunca viola a constraint.
--   * O trigger pages_set_transaction_time reescreve transaction_time=now() em todo
--     UPDATE; num UPDATE em massa isso só colidiria se houvesse 2 páginas VIVAS com
--     o mesmo (org, domain, slug). O PASSO 0 abaixo verifica essa invariante — se
--     retornar linhas, PARE e investigue antes de aplicar.
--   * Idempotente: só toca `project IS NULL`. Não mexe em chunks/embeddings
--     (project é atributo da page; a busca junta chunks→pages e filtra p.project).
--   * Multi-org: o join casa fonte e derivado pela MESMA organization_id.
--
-- ⚠️ N2 — muta pages.project. pg_dump ANTES. Rode PASSO 0 + PASSO 1 (read-only),
--    confira, e só então descomente e rode o PASSO 2.
--
-- RLS: `pages` tem FORCE ROW LEVEL SECURITY (migration 0008). Para rodar
-- CROSS-ORG num único statement, conecte como a role `nexus_service` (BYPASSRLS).
-- Para limitar a UMA org (recomendado no dogfood = org 2), rode antes, na MESMA
-- sessão, `SELECT set_config('nexus.org_id', '2', false);` — aí o app-user já
-- enxerga/atualiza só a org 2, e o join fonte↔derivado fica naturalmente escopado.

-- ─────────────────────────────────────────────────────────────────────────────
-- PASSO 0 — GUARDA (read-only): tem de retornar ZERO linhas. Se retornar, existem
-- páginas vivas duplicadas por (org, domain, slug) e o UPDATE em massa poderia
-- colidir no transaction_time — não aplique até resolver.
-- ─────────────────────────────────────────────────────────────────────────────
SELECT organization_id, domain, slug, count(*) AS vivas
FROM pages
WHERE valid_to IS NULL
GROUP BY organization_id, domain, slug
HAVING count(*) > 1;

-- ─────────────────────────────────────────────────────────────────────────────
-- PASSO 1 — PREVIEW (read-only): quantos derivados globais seriam reatribuídos, por
-- org e destino, e quantos permanecem globais por fontes mistas/sem project.
-- ─────────────────────────────────────────────────────────────────────────────
WITH src AS (
    SELECT d.id AS page_id,
           d.organization_id AS org,
           s.project AS src_project
    FROM pages d
    CROSS JOIN LATERAL jsonb_array_elements_text(d.metadata->'source_page_ids') AS sid(v)
    JOIN pages s ON s.id = sid.v::bigint AND s.organization_id = d.organization_id
    WHERE d.source_type = 'compiled' AND d.project IS NULL AND d.valid_to IS NULL
),
resolved AS (
    SELECT page_id, org,
           CASE WHEN count(*) = count(src_project)      -- nenhuma fonte global
                 AND count(DISTINCT src_project) = 1      -- todas do mesmo project
                THEN max(src_project) END AS proj
    FROM src
    GROUP BY page_id, org
)
SELECT org,
       COALESCE(proj, '(permanece global — fontes mistas/sem project)') AS destino,
       count(*) AS paginas
FROM resolved
GROUP BY org, destino
ORDER BY org, destino;

-- ─────────────────────────────────────────────────────────────────────────────
-- PASSO 2 — APPLY: descomente e rode APÓS PASSO 0 = 0 linhas, PREVIEW conferido e
-- BACKUP feito. Roda em transação; confira a contagem antes do COMMIT.
-- ─────────────────────────────────────────────────────────────────────────────
-- BEGIN;
-- WITH src AS (
--     SELECT d.id AS page_id, s.project AS src_project
--     FROM pages d
--     CROSS JOIN LATERAL jsonb_array_elements_text(d.metadata->'source_page_ids') AS sid(v)
--     JOIN pages s ON s.id = sid.v::bigint AND s.organization_id = d.organization_id
--     WHERE d.source_type = 'compiled' AND d.project IS NULL AND d.valid_to IS NULL
-- ),
-- resolved AS (
--     SELECT page_id,
--            CASE WHEN count(*) = count(src_project)
--                  AND count(DISTINCT src_project) = 1
--                 THEN max(src_project) END AS proj
--     FROM src
--     GROUP BY page_id
-- )
-- UPDATE pages d
-- SET project = r.proj
-- FROM resolved r
-- WHERE d.id = r.page_id AND r.proj IS NOT NULL;
--
-- -- Sanidade pós-update (antes do COMMIT):
-- SELECT count(*) FILTER (WHERE project IS NOT NULL) AS derivados_com_projeto,
--        count(*) FILTER (WHERE project IS NULL)     AS derivados_globais
-- FROM pages WHERE source_type='compiled' AND valid_to IS NULL;
--
-- COMMIT;
