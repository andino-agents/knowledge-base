# Getting Started

## Installation

Not on a package index: install from a release tag (binaries also attached
to each [GitHub Release](https://github.com/andino-agents/knowledge-base/releases)).

```bash
go install github.com/andino-agents/knowledge-base/cmd/andino-kb@v0.5.0
```

Or build from a clone (Go >= 1.26, `CGO_ENABLED=0`, nothing else):

```bash
git clone https://github.com/andino-agents/knowledge-base
cd knowledge-base
make build          # bin/andino-kb, version stamped from git describe
```

Releases cover linux and darwin, amd64 and arm64.

## CLI

```bash
andino-kb version                          # linker-stamped version, or "dev"
andino-kb validate -c config.yaml          # load + strict-parse + validate
andino-kb doctor   -c config.yaml          # config, backends, stores, sources
andino-kb index    -c config.yaml          # one-shot sync of every source
andino-kb search   -c config.yaml "query"  # debug hybrid search on stdout
andino-kb serve    -c config.yaml          # REST + MCP + /metrics
andino-kb eval                             # recall@k / MRR against a running server
andino-kb parity                           # same, vs a legacy RAG endpoint
```

`-c` / `--config` defaults to `config.yaml` and is persistent.

| Command | Flags |
|---|---|
| `index` | `--kb NAME` (one KB), `--rebuild` (drop the SQLite file first), `--wait DURATION` (default 10m) |
| `search` | `--kb NAME`, `--limit N` (default 8), `--min-score 0..1` |
| `serve` | `--wait DURATION` (default 10m) for embeddings and chat at startup |
| `eval` | `--queries`, `--url`, `--kb`, `--top`, `--json`, `--a` / `--b` (see [Evals](evals.md)) |
| `parity` | `--queries`, `--andino-url`, `--legacy-url`, `--kb`, `--top` |

`eval` and `parity` talk to a **running** `serve`. They do not open the
store themselves, and they do not send a bearer token — they only work
against an unauthenticated server. See [Evals](evals.md).

`--rebuild` deletes `<data_dir>/<kb>.db` (and `-wal`/`-shm`) and re-embeds
from scratch. It is a SQLite operation. For postgres, drop the KB schema
yourself — the command does not touch it. See
[Deployment](deployment.md#re-embedding).

## First knowledge base

`config.example.yaml` is the annotated template. A minimal file that
indexes a local folder:

```yaml
server:
  bind: "127.0.0.1:8180"
  data_dir: "./data"

inference:
  backends:
    - name: local-llama
      base_url: "http://127.0.0.1:8080/v1"
  embedding_models:
    - name: qwen3-embed
      backend: local-llama
      model: "qwen3-embedding-0.6b"
      dimensions: 1024

defaults:
  embedding_model: qwen3-embed

knowledge_bases:
  - name: notes
    sources:
      - name: vault
        type: localdir
        path: "/home/user/vault"
        include: ["**/*.md"]
        exclude: [".obsidian/**"]
        watch: true
```

Config is strict: unknown fields are startup errors, not silent no-ops.
`${VAR}` expands from the environment; an unset variable used as an API
key fails validation instead of quietly disabling the key.

```bash
andino-kb validate -c config.yaml
andino-kb doctor   -c config.yaml
andino-kb index    -c config.yaml
andino-kb serve    -c config.yaml
```

`doctor` walks the whole chain — config load, one real embedding (and
rerank / chat / vision if you configured them), store open, source listing
— and prints `fix:` next to each failure. Use it when "it doesn't work on
my setup" before opening an issue.

`serve` binds immediately. `/readyz` reports per-KB progress while the
process waits for inference and runs the initial sync. Queries against a
KB that is not ready still hit the store; readiness is the signal that
the first sync finished.

Full YAML: [Configuration](configuration.md).

## MCP clients

The MCP transport is stateless streamable HTTP at `/mcp`.

**Claude Code**

```bash
claude mcp add --transport http kb http://127.0.0.1:8180/mcp
```

**Cursor / other HTTP MCP clients** — point them at
`http://127.0.0.1:8180/mcp`. If you set `server.api_keys`, send
`Authorization: Bearer <key>`.

**andino agent-runtime**, in `agent.yaml`:

```yaml
mcp_servers:
  - name: kb
    transport: streamable_http
    url: "http://127.0.0.1:8180/mcp"
    headers: { Authorization: "Bearer ${ANDINO_KB_API_KEY}" }
```

**opencode**, in `opencode.json`:

```json
{
  "mcp": {
    "kb": { "type": "remote", "url": "http://127.0.0.1:8180/mcp" }
  }
}
```

Tools and REST: [API](api.md). Putting this on a network:
[Deployment](deployment.md).
