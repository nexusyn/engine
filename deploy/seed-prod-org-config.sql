-- seed-prod-org-config.sql — config de modelo da org de produção (org1).
-- Ver docs/VALIDATED-CONFIG-2026-05-29.md.
--
-- POR QUE NÃO É UMA MIGRATION: org_model_config referencia organizations(id);
-- numa DB fresca a org ainda não existe → FK falha no migrate. E é dado de
-- TENANT, não schema. Então: seed manual idempotente, rodar APÓS a org existir.
--
-- QUANDO RODAR: pós-restore de DB (um dump antigo reverte org_model_config),
-- ou ao provisionar a org de prod. Idempotente (ON CONFLICT DO UPDATE).
--
-- USO:
--   docker exec -i nexus-postgres psql -U nexus -d nexus < deploy/seed-prod-org-config.sql
--
-- A metade .env da config (extração + embed + rerank + chaves) NÃO vive aqui —
-- vive no .env (ver .env.example). Esta seed cobre só a GERAÇÃO (org_model_config).

-- Geração: MiniMax-M2.7 via provider nativo "minimax" (api.minimax.io, usa
-- MINIMAX_API_KEY do .env). ESCOLHIDO 2026-05-31: MiniMax é pago/flat (custo
-- marginal ZERO) → resolve a margem do pipeline. LongMemEval-S n=96: 81.2%
-- (vs gemini-3.5-flash 87.5% per-token). Acima de baselines publicos (67/75). Déficit em
-- knowledge-update/temporal é refinamento de prompt (tunado pro gemini) — a
-- recuperar. ALTERNATIVA paga de maior accuracy: provider='gemini' model='gemini-3.5-flash'.
INSERT INTO org_model_config (organization_id, stage, provider, model)
SELECT 1, 'generation', 'minimax', 'MiniMax-M2.7'
WHERE EXISTS (SELECT 1 FROM organizations WHERE id = 1)
ON CONFLICT (organization_id, stage)
DO UPDATE SET provider = 'minimax', model = 'MiniMax-M2.7', updated_at = now();

SELECT organization_id, stage, provider, model FROM org_model_config WHERE organization_id = 1;
