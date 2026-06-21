# NEXUS

> Memória persistente para agentes de IA. Single binary Go, Postgres only, MCP nativo.

**Status:** v2 em desenvolvimento — Semana 1 / 10-12. mnemonexus (v1, PHP) ainda em produção em paralelo.

---

## O que é

NEXUS é uma camada de memória persistente para agentes de IA:

- **Ingest** de conhecimento (sources, conversações, fatos)
- **Search híbrido** (vetorial + FTS + entity graph) sub-segundo
- **Query/answer** com LLM grounded em contexto recuperado
- **Compile Karpathy** (semantic compression on-demand) — diferencial
- **Lessons + decisions** first-class com twice-rule
- **MCP nativo** — Claude Code / Cursor conectam sem ponte
- **Multi-tenant** com Row-Level Security do Postgres

## Stack

| Camada | Tech |
|---|---|
| Linguagem | Go 1.25+ |
| HTTP | chi |
| DB | Postgres 17 + pgvector + pgvectorscale + tsvector pt-BR |
| Background jobs | River (Postgres-backed, sem Redis) |
| MCP | modelcontextprotocol/go-sdk (oficial) |
| Embedding | Jina embeddings v5 text-small (1024d) |
| Rerank | Jina reranker v3 |
| LLM | Gemini Flash (primary) + Anthropic Haiku + MiniMax (fallback) |
| SDKs | Python (`nexus-py`) + TypeScript (`@nexus/sdk`) |
| Observability | OpenTelemetry Go + GenAI semantic conventions |

## Estrutura do repo

Ver [`docs/01-repo-structure.md`](docs/01-repo-structure.md) para detalhes de cada pasta.

```
cmd/nexus/        # entrypoint
internal/         # core (não importável de fora)
sdk/              # nexus-py, @nexus/sdk
migrations/       # SQL versionado
docker/           # Dockerfiles
docs/             # documentação técnica
```

## Quick start (quando ready)

```bash
# Construir
make build

# Subir stack (Postgres + nexus)
docker compose up -d

# Rodar migrations
./bin/nexus migrate

# Iniciar API
./bin/nexus serve
```

## Documentação

| Doc | Descrição |
|---|---|
| [`docs/00-overview.md`](docs/00-overview.md) | Visão geral, tese, comparação com alternativas |
| [`docs/01-repo-structure.md`](docs/01-repo-structure.md) | Layout de pastas e convenções |
| [`docs/02-stack.md`](docs/02-stack.md) | Cada biblioteca em `go.mod` justificada |

## Roadmap

Plano completo em [`/Volumes/M5SSD/nexus/PLAN-V2-REWRITE-2026-05-20.md`](../PLAN-V2-REWRITE-2026-05-20.md) (10-12 semanas, decision points semanais).

## Licença

Privado por enquanto. Decisão de licença open-source pós v1.
