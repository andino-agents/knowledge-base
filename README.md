# andino-kb

Self-hosted knowledge bases for AI agents. One static binary that indexes
documents from declarative pipelines and serves hybrid search over MCP and
REST.

Think *AWS Bedrock Knowledge Bases, but on-prem, air-gapped-friendly, and
agent-first* — for coding agents that need shared knowledge inside a private
network, and for agent runtimes that need durable, searchable memory.

Licensed under [Apache-2.0](LICENSE).

```
 sources                     andino-kb                      consumers
┌──────────────┐      ┌──────────────────────┐      ┌──────────────────────┐
│ local folder  │ ──▶ │ incremental indexer   │      │ Claude Code / Cursor  │
│ git repo      │ ──▶ │ pdf/docx + OCR (VLM)  │      │  via MCP (HTTP)       │
│ s3 / minio    │ ──▶ │ SQLite / pgvector     │ ◀──▶ │ autonomous agents     │
│ agent writes  │ ──▶ │ hybrid + contextual   │      │  via MCP or REST      │
└──────────────┘      └──────────────────────┘      └──────────────────────┘
```

## Quick Start

```bash
# 1. Get a binary (or: make build)
go install github.com/andino-agents/knowledge-base/cmd/andino-kb@v0.5.0

# 2. Describe your knowledge bases
cp config.example.yaml config.yaml && $EDITOR config.yaml

# 3. Validate, diagnose, index, serve
andino-kb validate -c config.yaml
andino-kb doctor   -c config.yaml     # config, backends, stores, sources
andino-kb index    -c config.yaml     # one-shot sync (waits for inference)
andino-kb serve    -c config.yaml     # REST + MCP + /metrics on one port
```

Point any MCP client at `http://your-host:8180/mcp` (streamable HTTP). The
default bind is loopback and the transport is plain HTTP — see
[Deployment](docs/deployment.md#security) before exposing it to a network.

**Claude Code**

```bash
claude mcp add --transport http kb http://your-host:8180/mcp
```

**andino agent-runtime** (or any Strands-based agent), in `agent.yaml`:

```yaml
mcp_servers:
  - name: kb
    transport: streamable_http
    url: "http://your-host:8180/mcp"
    headers: { Authorization: "Bearer ${ANDINO_KB_API_KEY}" }
```

**opencode**, in `opencode.json`:

```json
{
  "mcp": {
    "kb": { "type": "remote", "url": "http://your-host:8180/mcp" }
  }
}
```

## Features

**Headless and agent-first**
- The product *is* the MCP/REST endpoint. No chat UI. Tool descriptions
  are written for agents.
- `store` / `search` / `get` / `list` / `delete` map 1:1 to the semantics of
  the Strands `memory` tool, so a Strands agent can swap Bedrock Knowledge
  Bases for andino-kb without changing its behavior.
- Bearer keys scoped `read` or `readwrite` govern both interfaces alike: a
  `read` key is not offered `store` or `delete_document` in `tools/list`.

**One static binary, two stores**
- Pure Go (`CGO_ENABLED=0`). No external services required. SQLite is the
  default: one file per knowledge base.
- Swap in a shared **pgvector** database (one schema per KB, tsvector +
  dense search) without touching the engine.

**Declarative pipelines**
- Sources are config, not clicks: a local directory (filesystem watcher),
  a git repository (shallow clone + poll), an S3/MinIO bucket (standard
  AWS credential chain), and agent writes into writable KBs.
- Formats: markdown, plaintext, code, PDF and DOCX. Optional OCR of
  scanned PDF pages through any vision-capable chat model.

**Hybrid retrieval that holds up**
- BM25 + dense vectors fused with Reciprocal Rank Fusion, optional
  cross-encoder reranking, optional [contextual
  retrieval](https://www.anthropic.com/engineering/contextual-retrieval)
  at index time.
- Per-request `rerank`, `max_per_doc`, metadata `filter`, and
  `rerank_score` on the wire so a caller can tell which path ordered the
  results.
- Inference against any OpenAI-compatible endpoint: llama.cpp, vLLM,
  Ollama, OpenAI, Bedrock.

**Operable on a VM**
- `andino-kb doctor` walks config, backends, stores and sources and prints
  the fix next to each failure.
- `/healthz` (liveness), `/readyz` (per-KB readiness), Prometheus
  `/metrics`, structured logs, graceful shutdown, example systemd unit.
- `andino-kb eval` and `andino-kb parity` gate retrieval changes and
  migrations on recall, not vibes.

## Documentation

- [Getting Started](docs/getting-started.md) — install, CLI, first knowledge base, MCP clients
- [Configuration](docs/configuration.md) — the YAML reference
- [API](docs/api.md) — REST endpoints and MCP tools
- [Retrieval & Evals](docs/evals.md) — hybrid search, contextual retrieval, measured numbers, `eval` / `parity`
- [Architecture](docs/architecture.md) — pipeline, stores, failure modes
- [Deployment](docs/deployment.md) — systemd, security, ops endpoints, postgres
- [Changelog](CHANGELOG.md) — what shipped, and why

## License

[Apache-2.0](LICENSE). Built by [andino-agents](https://github.com/andino-agents).
