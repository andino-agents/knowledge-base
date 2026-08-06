# andino-kb

**Self-hosted knowledge bases for AI agents.** One static binary that indexes
your documents from declarative pipelines and serves hybrid search to coding
agents and autonomous agents over MCP and REST.

Think *AWS Bedrock Knowledge Bases, but on-prem, air-gapped-friendly, and
agent-first* — for teams whose coding agents (Claude Code, opencode, Cursor)
need shared knowledge inside a private network, and for agent runtimes that
need durable, searchable memory.

```
 sources                     andino-kb                      consumers
┌──────────────┐      ┌──────────────────────┐      ┌──────────────────────┐
│ local folder  │ ──▶ │ incremental indexer   │      │ Claude Code / opencode│
│ git repo      │ ──▶ │ pdf/docx + OCR (VLM)  │      │  via MCP (HTTP)       │
│ s3 / minio    │ ──▶ │ SQLite (1 file/KB)    │ ◀──▶ │ autonomous agents     │
│ agent writes  │ ──▶ │ hybrid + contextual   │      │  via MCP or REST      │
└──────────────┘      └──────────────────────┘      └──────────────────────┘
```

## Why another RAG server?

The existing self-hosted options are chat platforms first (web UI, Docker
Compose, Elasticsearch + MySQL + Redis + MinIO, 16 GB of RAM before your
first query). andino-kb is the opposite trade:

- **Headless and agent-first** — the product *is* the MCP/REST endpoint.
  No chat UI. Tool descriptions are written for agents.
- **One static binary, one SQLite file per knowledge base** — pure Go
  (`CGO_ENABLED=0`), no external services, no daemons to babysit. Runs on a
  VM, a homelab, or an air-gapped host.
- **Declarative pipelines** — sources are config, not clicks: a local
  directory (with a filesystem watcher), a git repository (shallow clone +
  poll), an S3/MinIO bucket (standard AWS credential chain, custom
  endpoints), and agent writes into writable KBs. Formats: markdown,
  plaintext, code, PDF and DOCX — with optional OCR of scanned PDF pages
  through any vision-capable chat model (a local llama.cpp with vision, or
  a dedicated OCR VLM like
  [Unlimited-OCR](https://github.com/baidu/Unlimited-OCR) on vLLM).
- **Hybrid retrieval that holds up** — BM25 (FTS5) + dense vectors fused
  with Reciprocal Rank Fusion, optional cross-encoder reranking via any
  OpenAI-compatible `/v1/rerank`, and optional [contextual
  retrieval](https://www.anthropic.com/engineering/contextual-retrieval):
  an LLM situates every chunk within its document at index time, and that
  context is embedded and BM25-indexed with the text. Keyword-only misses
  paraphrases; vector-only misses exact identifiers; you want both.
- **Inference-backend agnostic** — embeddings and reranking against any
  OpenAI-compatible endpoint: llama.cpp, vLLM, Ollama, OpenAI, Bedrock.
- **Apache-2.0, all of it** — no open-core cut.

## Quick start

```bash
# 1. Get a binary (or: make build)
go install github.com/andino-agents/knowledge-base/cmd/andino-kb@latest

# 2. Describe your knowledge bases
cp config.example.yaml config.yaml && $EDITOR config.yaml

# 3. Validate, index, serve
andino-kb validate -c config.yaml
andino-kb index    -c config.yaml     # one-shot sync (waits for your embedding endpoint)
andino-kb serve    -c config.yaml     # REST + MCP + /metrics on one port
```

Point any MCP client at `http://your-host:8180/mcp` (streamable HTTP).

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

## MCP tools

| Tool | What it does |
|---|---|
| `search` | Hybrid search across one or all KBs; results carry document path, heading path and line span |
| `get_document` | Fetch a document, optionally sliced by lines — expand context around a hit |
| `list_knowledge_bases` / `list_documents` | Discovery and paging |
| `store` | Persist agent knowledge into a writable KB (returns a `memory_*` id) |
| `delete_document` | Remove a stored document |

`store`/`search`/`get`/`list`/`delete` map 1:1 to the semantics of the
Strands `memory` tool, so a Strands agent can swap Bedrock Knowledge Bases
for andino-kb without changing its behavior. The same operations exist as
REST endpoints (`/v1/kb/{kb}/...`) with bearer keys scoped `read` or
`readwrite`.

## Configuration

One YAML file: inference backends, knowledge bases, sources. See
[config.example.yaml](config.example.yaml) for the full annotated example.

```yaml
knowledge_bases:
  - name: team-docs
    sources:
      - name: docs-repo
        type: git
        url: "https://github.com/example/docs.git"
        paths: ["docs/**/*.md"]
        poll_interval: 5m
  - name: agent-memory
    writable: true            # agents store/delete here via MCP or REST
```

Config is strict: unknown fields are startup errors, not silent no-ops.

## Design notes (the hard-won parts)

andino-kb grew out of operating a vault RAG whose failure modes we got to
know intimately. They are design requirements here:

- **No dummy vectors, ever.** An embedding-endpoint error aborts that
  document and keeps the previous version; there is no fallback code path.
  (A dummy-vector fallback once convinced a dimension check to wipe an
  entire collection.)
- **Atomic writes are the normal case.** Editors and agents write via temp
  file + rename; the watcher never branches on event types — it marks paths
  dirty and re-stats reality.
- **Startup waits for the backend.** Local inference servers take minutes to
  load models after a reboot; `/readyz` reports per-KB progress and indexing
  starts only when `/v1/embeddings` truly answers.
- **Identity is sacred.** The embedding model and dimensions are recorded per
  KB; a mismatch is a hard error that never auto-wipes. Re-embedding is an
  explicit `andino-kb index --rebuild`.
- **Incremental sync that cannot lie.** A content-hash manifest drives
  reindexing, and a "no changes" answer is cross-checked against a non-empty
  index.

**Measured on a real 238-document knowledge base** (40 golden queries,
document-level metrics, local llama.cpp inference):

| Configuration | recall@1 | recall@5 | MRR | p50 latency |
|---|---|---|---|---|
| Hybrid (BM25 + vectors + RRF) | 75% | 98% | 0.848 | 24 ms |
| + cross-encoder rerank | 82% | 98% | 0.884 | ~850 ms |
| + **contextual retrieval** (no rerank) | **88%** | **98%** | **0.908** | **26 ms** |

Contextual retrieval lifted recall@1 by 13 points over plain hybrid — and
made reranking unnecessary for this corpus (`rerank_default: "off"` keeps
the reranker available per-request). Measure your own corpus with
`andino-kb eval`; retrieval changes here get gated on those numbers, not
vibes.

Vector search is a brute-force cosine scan held in memory (embeddings
persist as BLOBs in SQLite). At the supported scale — up to ~100k chunks per
KB — that is single-digit milliseconds in pure Go with zero moving parts;
measured: **14 ms per hybrid query over 10k chunks × 1024 dims**, ~30 ms
end-to-end including query embedding on a local llama.cpp. The storage layer
is an interface: pgvector, OpenSearch and S3 Vectors providers can register
without touching the engine.

## Operations

`/healthz`, `/readyz` (per-KB startup state), Prometheus `/metrics`
(search latency histograms, index counters, watcher events), structured
logs, graceful shutdown, an example systemd unit in [deploy/](deploy/), and
`andino-kb parity` — a golden-query harness to gate a migration from an
existing RAG on recall.

## Roadmap

- pgvector and OpenSearch storage providers
- Native Strands tool package in the andino agent-runtime

## License

[Apache-2.0](LICENSE). Built by [andino-agents](https://github.com/andino-agents).
