# Deployment

The primary target is a **small VM**: one static binary, one config file,
one data directory. SQLite needs nothing else. Postgres is a shared
database you already run.

Why a VM and not a cluster:

- All SQLite state is files under `server.data_dir`. Backup = copy the
  directory (stop the process, or copy the `.db` after a checkpoint).
- One process, one port, in-memory reindex jobs and readiness map: a
  second replica does not share that state.
- Inference is usually a local llama.cpp / vLLM on the same host or a
  sibling. The indexer waits for it; it does not discover it.

Releases are linux and darwin, amd64 and arm64, `CGO_ENABLED=0`, attached
to each [GitHub Release](https://github.com/andino-agents/knowledge-base/releases).

## systemd

An example user unit ships in [deploy/andino-kb.service](../deploy/andino-kb.service):

```bash
# 1. Binary and config
install -m 0755 andino-kb ~/.local/bin/andino-kb
mkdir -p ~/.config/andino-kb ~/.local/share/andino-kb
cp config.example.yaml ~/.config/andino-kb/config.yaml
# set server.data_dir to ~/.local/share/andino-kb
install -m 0600 /dev/null ~/.config/andino-kb/env
# ANDINO_KB_API_KEY=... and any token_env / AWS vars

# 2. Unit
cp deploy/andino-kb.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now andino-kb
sudo loginctl enable-linger "$USER"
```

The example unit is intentionally thin. For a system-wide install, add
hardening (`ProtectSystem=strict`, `ReadWritePaths=<data_dir>`,
`ReadOnlyPaths=<sources>`) and run it as a dedicated user.

`Restart=on-failure` is enough: `serve` binds immediately and retries
the initial sync. A model that takes three minutes to load after reboot
is a readiness problem, not a crash loop.

## Security

**Without `server.api_keys`, the server is open.** Every REST endpoint
and every MCP tool answers any caller that can reach the port. That is
deliberate for the loopback default (`127.0.0.1:8180`) and the wrong
default the moment you change `bind`.

```yaml
server:
  bind: "127.0.0.1:8180"
  api_keys:
    - key: "${ANDINO_KB_READ_KEY}"
      scope: read
    - key: "${ANDINO_KB_ADMIN_KEY}"
      scope: readwrite
```

Keys are `Authorization: Bearer <key>`, compared in constant time.
Expand them from the environment; an unset variable fails validation
instead of quietly disabling a key.

Scopes apply to REST and MCP equally. A `read` key is not offered
`store` or `delete_document` in MCP `tools/list`. A KB's `writable:
true` is a second gate: both must agree.

There is no TLS and no per-client identity. Terminate TLS in a reverse
proxy if the traffic leaves the host. Do not put andino-kb on a public
address without a proxy and keys.

S3 credentials never go in the YAML. They come from the standard AWS
chain (env, shared config, SSO, IRSA, instance profiles). Git tokens
are an env-var *name* (`token_env: GH_TOKEN`), not a value.

## Ops endpoints

| Endpoint | Auth | Meaning |
|---|---|---|
| `GET /healthz` | always open | Liveness. Returns `ok` even if every KB is unready. |
| `GET /readyz` | `ready` boolean always open; per-KB detail behind a key when `ops_require_auth` applies | `200` when every KB finished its initial sync; `503` otherwise |
| `GET /metrics` | behind a key when `ops_require_auth` applies | Prometheus: `andino_search_duration_seconds`, `andino_index_operations_total`, `andino_watcher_events_total` |

`server.ops_require_auth` defaults to on whenever `api_keys` is set,
because `/metrics` and the per-KB detail of `/readyz` name your
knowledge bases and count their documents. Set it to `false` if an
unauthenticated Prometheus needs to scrape.

`/healthz` is the wrong probe for "can I search". Use `/readyz`.

Graceful shutdown: SIGINT / SIGTERM, 15s `Shutdown` timeout. Watchers
and pollers die with the context. In-flight reindex goroutines are
detached from the request and will be killed with the process.

Logs are structured (`text` or `json`) on stderr. `journalctl --user -u
andino-kb` is the usual place.

## Postgres

```yaml
storage:
  provider: postgres
  options:
    dsn: "postgres://user:pass@localhost:5432/andino_kb?sslmode=disable"
```

The process creates one schema per KB on first open, sets `search_path`
to that schema, and tries `CREATE EXTENSION IF NOT EXISTS` for both
`pgvector` and `vector` (installs disagree on the name), pinned to
`SCHEMA public` so every KB can see the type. The role needs permission
to create extensions and schemas.

The HNSW index is best-effort: if this build of pgvector cannot create
it, dense search falls back to a sequential scan and a warning is
logged. Results stay correct; they get slower.

Backup is the database's backup. Several KBs on one DSN is the point;
do not give each KB its own database unless you want to.

## Re-embedding

The store records the embedding model and dimensions on first open. A
mismatch is a hard error. Nothing auto-wipes.

| Provider | How to rebuild |
|---|---|
| `sqlite` | `andino-kb index --rebuild` (optionally `--kb NAME`) deletes `<data_dir>/<kb>.db` and the WAL/SHM, then re-embeds |
| `postgres` | drop the KB schema yourself (`DROP SCHEMA <kb> CASCADE`), then `andino-kb index`. `--rebuild` does **not** touch postgres |

`--rebuild` only unlinks SQLite files. The error message on a postgres
identity mismatch still says "run `andino-kb index --rebuild`"; that
command will not drop the schema. Drop it, then index.

## Inference on the same host

`serve` and `index` wait up to `--wait` (default 10m) for embeddings
and, if contextual retrieval is on, for chat. A llama.cpp router loads
those as two models. Order the wait, do not skip it — see
[Architecture](architecture.md#inference-readiness).

`andino-kb doctor` is the first thing to run after a reboot: it probes
embeddings, rerank, chat, vision, stores and sources and prints the fix
next to each failure.
