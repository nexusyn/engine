-- Telemetria do knowledge graph (entities/edges) — baseline GraphRAG.
-- Leitura pura. Rodar contra o Postgres do engine:
--   docker exec -i <postgres> psql -U <user> -d <db> < scripts/graph_telemetry.sql
-- Usuário `nexus` é superuser+bypassrls → enxerga todas as orgs.
-- Baseline 2026-06-18 documentado em docs/plan-2026-06-18-graphrag-fase0-baseline.md
\pset pager off

\echo '==== 1. VOLUME POR ORG (pages/entities/edges) ===='
SELECT o.id AS org, o.slug,
       (SELECT count(*) FROM pages p    WHERE p.organization_id=o.id) AS pages,
       (SELECT count(*) FROM entities e WHERE e.organization_id=o.id) AS entities,
       (SELECT count(*) FROM edges g    WHERE g.organization_id=o.id) AS edges
FROM organizations o
ORDER BY entities DESC, edges DESC;

\echo ''
\echo '==== 2. ENTITIES POR KIND (global) — espera vocabulario controlado ===='
SELECT kind, count(*) AS n, count(*) FILTER (WHERE valid_to IS NOT NULL) AS superseded
FROM entities GROUP BY kind ORDER BY n DESC;

\echo ''
\echo '==== 3. EDGES POR KIND (global) — vigia explosao de vocabulario ===='
SELECT kind, count(*) AS n,
       round(avg(weight)::numeric,3) AS avg_weight,
       count(*) FILTER (WHERE valid_to IS NOT NULL) AS superseded
FROM edges GROUP BY kind ORDER BY n DESC LIMIT 60;

\echo ''
\echo '==== 3b. CONCENTRACAO DO VOCABULARIO DE EDGES ===='
WITH k AS (SELECT kind, count(*) n FROM edges GROUP BY kind)
SELECT count(*) AS kinds_distintos, sum(n) AS total_edges,
       count(*) FILTER (WHERE n=1)  AS kinds_com_1,
       count(*) FILTER (WHERE n<=2) AS kinds_ate_2,
       round(100.0*sum(n) FILTER (WHERE n>=100)/sum(n),1) AS pct_edges_top_kinds,
       round(100.0*count(*) FILTER (WHERE n<=2)/count(*),1) AS pct_kinds_cauda_longa
FROM k;

\echo ''
\echo '==== 4. EDGES ORFAS (FK quebrada) — espera 0 ===='
SELECT count(*) FILTER (WHERE ef.id IS NULL) AS from_orfa,
       count(*) FILTER (WHERE et.id IS NULL) AS to_orfa,
       count(*) FILTER (WHERE g.source_page_id IS NOT NULL AND sp.id IS NULL) AS source_page_orfa
FROM edges g
LEFT JOIN entities ef ON ef.id=g.from_entity_id
LEFT JOIN entities et ON et.id=g.to_entity_id
LEFT JOIN pages sp    ON sp.id=g.source_page_id;

\echo ''
\echo '==== 5. DISTRIBUICAO DE GRAU (org :org) — agregacao eficiente ===='
\set org 2
WITH endpoints AS (
  SELECT from_entity_id AS eid FROM edges WHERE organization_id=:org
  UNION ALL SELECT to_entity_id FROM edges WHERE organization_id=:org
),
deg AS (
  SELECT e.id, coalesce(d.c,0) AS degree
  FROM entities e
  LEFT JOIN (SELECT eid, count(*) c FROM endpoints GROUP BY eid) d ON d.eid=e.id
  WHERE e.organization_id=:org
)
SELECT count(*) AS entities, count(*) FILTER (WHERE degree=0) AS grau_zero,
       round(avg(degree)::numeric,2) AS grau_medio, max(degree) AS grau_max,
       percentile_disc(0.5)  WITHIN GROUP (ORDER BY degree) AS p50,
       percentile_disc(0.90) WITHIN GROUP (ORDER BY degree) AS p90,
       percentile_disc(0.99) WITHIN GROUP (ORDER BY degree) AS p99
FROM deg;

\echo ''
\echo '==== 6. TOP 20 SUPER-HUBS (org :org) — candidatos a hub-dampening ===='
WITH endpoints AS (
  SELECT from_entity_id AS eid FROM edges WHERE organization_id=:org
  UNION ALL SELECT to_entity_id FROM edges WHERE organization_id=:org
)
SELECT e.name, e.kind, count(*) AS degree
FROM endpoints ep JOIN entities e ON e.id=ep.eid
GROUP BY e.id, e.name, e.kind ORDER BY degree DESC LIMIT 20;

\echo ''
\echo '==== 7. COBERTURA DO EXTRACTOR (pages memory/knowledge) ===='
SELECT o.id AS org, o.slug,
  count(*) FILTER (WHERE p.domain IN ('memory','knowledge')) AS mem_pages,
  count(*) FILTER (WHERE p.domain IN ('memory','knowledge') AND p.entities_extracted_at IS NOT NULL) AS extraidas,
  count(*) FILTER (WHERE p.domain IN ('memory','knowledge') AND p.entities_extracted_at IS NULL)     AS pendentes
FROM organizations o JOIN pages p ON p.organization_id=o.id
GROUP BY o.id, o.slug HAVING count(*) FILTER (WHERE p.domain IN ('memory','knowledge'))>0
ORDER BY mem_pages DESC;

\echo ''
\echo '==== 8. CANDIDATOS A DEDUP (org :org) — nomes quase-iguais, mesma kind ===='
SELECT a.kind, a.name AS nome_a, b.name AS nome_b,
       round(similarity(a.name,b.name)::numeric,2) AS sim
FROM entities a JOIN entities b
  ON a.organization_id=:org AND b.organization_id=:org
  AND a.kind=b.kind AND a.id<b.id
  AND a.name % b.name AND similarity(a.name,b.name) BETWEEN 0.55 AND 0.999
ORDER BY sim DESC LIMIT 25;
