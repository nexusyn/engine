#!/usr/bin/env bash
# Runner de teste pontual LongMemEval por categoria.
#
#   bash run.sh <tipo> <sample> [seed]
#
# tipo  = single-session-preference | temporal-reasoning | multi-session |
#         knowledge-update | single-session-user | single-session-assistant | all
# sample= nº de casos (para um tipo só, use o total: preference=30, assistant=56,
#         demais=133; "all" usa sample estratificado, ex 400 → 350 com cap por tipo)
#
# Variância: a geração roda a temp 0.2 → ±1-3 casos de ruído. Para mudanças
# pequenas, rode 2-3x e compare a média (1 caso de preference = 3,3pp).
#
# Pré-requisitos: stack do bench no ar (docker compose -f docker-compose.yml up -d --build),
# .env preenchido, e o dataset em $LME_DATA. Veja README.md.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

TYPE="${1:-single-session-preference}"
SAMPLE="${2:-30}"
SEED="${3:-42}"

export LME_API="${LME_API:-http://127.0.0.1:8055}"
export LME_DATA="${LME_DATA:-$HERE/data/longmemeval_s_cleaned.json}"
export LME_LIMIT="${LME_LIMIT:-20}"
export LME_MODE="${LME_MODE:-hybrid}"
export LME_SEED="$SEED"
export LME_SAMPLE="$SAMPLE"
export LME_PG_CONTAINER="${LME_PG_CONTAINER:-nexus-bench-postgres}"
# Juiz: openrouter/gemini-2.5-flash é o do baseline (comparar SEMPRE com o mesmo juiz).
export LME_JUDGE_PROVIDER="${LME_JUDGE_PROVIDER:-openrouter}"
export LME_JUDGE_OR_MODEL="${LME_JUDGE_OR_MODEL:-google/gemini-2.5-flash}"
[ "$TYPE" != "all" ] && export LME_TYPE="$TYPE"

# Token + chaves: do ambiente, ou puxa do container do bench se disponível.
: "${NEXUS_V2_TOKEN:=${NEXUS_API_TOKEN:-}}"
if [ -z "${NEXUS_V2_TOKEN}" ] && [ -f "$HERE/token.txt" ]; then NEXUS_V2_TOKEN="$(cat "$HERE/token.txt")"; fi
if [ -z "${NEXUS_V2_TOKEN}" ]; then NEXUS_V2_TOKEN="$(docker exec nexus-bench-nexus printenv NEXUS_API_TOKEN 2>/dev/null || true)"; fi
export NEXUS_V2_TOKEN
: "${OPENROUTER_API_KEY:=$(docker exec nexus-bench-nexus printenv OPENROUTER_API_KEY 2>/dev/null || true)}"
: "${ANTHROPIC_API_KEY:=$(docker exec nexus-bench-nexus printenv ANTHROPIC_API_KEY 2>/dev/null || true)}"
export OPENROUTER_API_KEY ANTHROPIC_API_KEY

[ -z "${NEXUS_V2_TOKEN}" ] && { echo "ERRO: defina NEXUS_V2_TOKEN (token do engine de bench)"; exit 1; }
echo "POINTED TEST $(date -u +%FT%TZ) type=$TYPE sample=$SAMPLE seed=$SEED limit=$LME_LIMIT judge=$LME_JUDGE_OR_MODEL repeat=${LME_REPEAT:-1} panel=${LME_JUDGE_PANEL:-0}"
# LME_REPEAT>1 → modo replicação (média ± desvio sobre N runs idênticos, isola o
# ruído de geração). Senão, run único.
if [ "${LME_REPEAT:-1}" -gt 1 ] 2>/dev/null; then
  exec python3 -u "$HERE/lme_replicate.py"
else
  exec python3 -u "$HERE/lme_bench.py"
fi
