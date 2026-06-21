-- 0032_edge_confidence.sql — Fase 2 GraphRAG: provenance por aresta.
-- O campo `weight` é constante (≈1.0 em prod, 700 kinds) → não carrega sinal de
-- confiança. Adiciona confidence tri-estado (extracted/inferred/ambiguous) +
-- score, pra rankear/gate a expansão de grafo no retrieval (Fase 1).
-- Legado: ADD ... DEFAULT preenche as edges existentes como 'extracted'/1.0
-- (o extractor antigo só extraía relações explícitas — assunção conservadora).

-- +goose Up
-- +goose StatementBegin
ALTER TABLE edges
    ADD COLUMN IF NOT EXISTS confidence TEXT NOT NULL DEFAULT 'extracted',
    ADD COLUMN IF NOT EXISTS confidence_score REAL NOT NULL DEFAULT 1.0;

-- Valida o estado tri-estado e o range do score.
ALTER TABLE edges DROP CONSTRAINT IF EXISTS edges_confidence_chk;
ALTER TABLE edges ADD CONSTRAINT edges_confidence_chk
    CHECK (confidence IN ('extracted', 'inferred', 'ambiguous')
           AND confidence_score > 0 AND confidence_score <= 1);

COMMENT ON COLUMN edges.confidence IS
    'Provenance: extracted (dito no texto) | inferred (implícito) | ambiguous (incerto). Fase 2 GraphRAG.';
COMMENT ON COLUMN edges.confidence_score IS
    'Confiança na aresta em (0,1]. ≈1.0 extracted, ≈0.7 inferred, ≈0.4 ambiguous.';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE edges DROP CONSTRAINT IF EXISTS edges_confidence_chk;
ALTER TABLE edges DROP COLUMN IF EXISTS confidence_score;
ALTER TABLE edges DROP COLUMN IF EXISTS confidence;
-- +goose StatementEnd
