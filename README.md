<div align="center">

# Nexusyn Engine

**Open-source long-term, time-aware memory engine for AI agents.**

[![License: Apache 2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.27-00ADD8.svg)](go.mod)
[![Security Policy](https://img.shields.io/badge/Security-Policy-green.svg)](SECURITY.md)

[Documentation](https://nexusyn.ai/docs) • [Cloud API](https://nexusyn.ai) • [Benchmarks](https://nexusyn.ai/#benchmark)

</div>

---

## What is Nexusyn?

Most memory layers for AI agents simply dump nearest-neighbor vectors into the prompt. **Nexusyn** goes further by providing a structured, bi-temporal memory engine with:

- **Bi-Temporal Recall:** Every memory is versioned over time (`valid_from`/`valid_to`). Updates supersede older facts rather than overwriting them, preserving historical accuracy.
- **Hybrid Retrieval:** Dense vector search (HNSW on `pgvector`) + BM25 full-text + entity graph signals fused with Reciprocal Rank Fusion (RRF) and reranking.
- **Grounded Answers:** Returns synthesized answers with verifiable citations and source chunks.
- **MCP-Native:** First-class Model Context Protocol server (`/v1/mcp`) for seamless integration with Claude Code, Cursor, and custom agents.
- **Strict Tenancy:** Multi-tenant database isolation using PostgreSQL Row-Level Security (RLS).
- **Asynchronous Pipeline:** Background chunking, entity extraction, and wiki compilation powered by [River](https://riverqueue.com).

## Benchmark Results

Nexusyn is measured against public long-term memory benchmarks:

| Benchmark | Accuracy | Description |
|---|---|---|
| **LongMemEval-S** | **81.1%** (284/350) | Multi-session recall and reasoning across temporal horizons |
| **LoCoMo** | **74.3%** (1476/1986) | Complex conversational memory and aggregation |

---

## Quickstart (Docker Compose)

The easiest way to run Nexusyn locally with PostgreSQL (`pgvector`), the API server, and the background worker:

```bash
# 1. Clone repository
git clone https://github.com/nexusyn/engine.git
cd engine

# 2. Configure environment
cp .env.example .env
# Edit .env and set your preferred LLM and embedding provider API keys

# 3. Start stack
docker compose up -d

# 4. Check health
curl http://localhost:8044/health
```

---

## Agent Integration via MCP

Nexusyn provides a native HTTP Model Context Protocol server at `/v1/mcp`.

### Claude Code
```bash
claude mcp add nexusyn --transport http \
  "http://localhost:8044/v1/mcp" \
  --header "Authorization: Bearer <YOUR_API_TOKEN>"
```

### Cursor (`~/.cursor/mcp.json`)
```json
{
  "mcpServers": {
    "nexusyn": {
      "url": "http://localhost:8044/v1/mcp",
      "headers": {
        "Authorization": "Bearer <YOUR_API_TOKEN>"
      }
    }
  }
}
```

The agent automatically gets access to `add_memory`, `search_memory`, `get_guideline`, `update_memory`, and `delete_memory`.

---

## REST API Overview

Two core endpoints are all that is required for standard usage:

### 1. Ingest a memory
```bash
curl -X POST http://localhost:8044/v1/ingest \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "title": "Architecture Decision",
    "content": "We chose PostgreSQL 18 with pgvector for HNSW indexing on the memory table.",
    "agent": "claude",
    "project": "core-backend"
  }'
```

### 2. Query grounded memory
```bash
curl -X POST http://localhost:8044/v1/query \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "question": "What database and indexing strategy did we choose for memory?",
    "project": "core-backend"
  }'
```

---

## Hosted Cloud & Managed Platform

Don't want to manage PostgreSQL, background workers, and vector scaling?

Try **[Nexusyn Cloud](https://nexusyn.ai)** — generous free tier, global latency routing, 3D memory graph explorer, and zero operational maintenance.

---

## License & Trademark

- **Code:** Licensed under the [Apache License 2.0](LICENSE).
- **Trademark:** "Nexusyn" and associated logos are trademarks of RedFoxCode.

See [SECURITY.md](SECURITY.md) for vulnerability reporting.
