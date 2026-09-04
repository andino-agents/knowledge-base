# Architecture

```
config.yaml
    │
    ▼
andino-kb serve / index
    │
    ├── inference.Embedder  ── OpenAI-compatible /v1/embeddings
    ├── inference.Chat      ── /v1/chat/completions (contextual, OCR)
    ├── inference.Reranker  ── /v1/rerank (optional, query time)
    │
    └── per knowledge base
          ├── store.Store          sqlite | postgres
          ├── pipeline.Indexer     extract → chunk → (OCR) → (contextual) → embed → upsert
          └── source.Source[]      localdir | git | s3 | (managed writes)
```

The HTTP server comes up immediately. Each KB waits for its embedding
backend (and its chat backend, if contextual retrieval is on), runs the
initial sync, then starts watchers and pollers. `/readyz` is that
progress.

## Indexing pipeline

`Indexer.SyncSource` is incremental and driven by a content-hash
manifest:

1. `source.Sync` (git fetch / no-op for localdir and s3).
2. `source.List` — only extractor-known extensions.
3. Diff against `store.Manifest`. Unchanged hash → `TouchManifest` if
   size/mtime moved, otherwise skip. Missing from the listing → delete.
4. For each changed path: extract → chunk → optional OCR of scanned PDF
   pages → optional contextual retrieval → embed → `UpsertDocument`.
   Agent `store` (`IndexManaged`) is markdown + embed only: no OCR, no
   contextual retrieval. Without OCR enabled, scanned PDF pages are
   skipped (a warning), not failed.

`UpsertDocument` and `DeleteDocument` are atomic. A failure leaves the
previous version of the document fully intact. There is **no dummy
vector path**: an embedding error aborts that document and keeps what
was there. (A dummy-vector fallback once convinced a dimension check to
wipe an entire collection.)

`Manifest` refuses to report entries over a store with zero chunks: a
non-empty manifest on an empty index would make the next sync trust
"no changes" and skip everything. The store returns an empty manifest
instead, so the indexer re-embeds.

The watcher never branches on event types. Editors and agents write via
temp file + rename; the watcher marks the path dirty, a per-path
debounce absorbs the burst, and `SyncPath` re-stats reality (reindex or
delete).

Git and S3 poll on `poll_interval`. The initial sync retries the whole
pass up to 5 times with a long backoff (15s, 30s, 1m, 2m, 2m): what it
waits out is a model load, not a blip. Already-indexed documents are
skipped by the manifest, so a retry is cheap. `400 "model is not
loaded"` from a router at its model cap is **not** retried by
`Chat.Complete` (only 5xx and 429); the sync-level retry is what rides
that out.

`POST /v1/kb/{kb}/reindex` is the same incremental `SyncSource`, in the
background. It is not `--rebuild`.

## Identity

The embedding model name and dimensions are recorded in the store on
first open (`kb_meta`). A later mismatch is a hard error that never
auto-wipes. Re-embedding is explicit: drop the SQLite file with
`andino-kb index --rebuild`, or drop the postgres schema yourself. See
[Deployment](deployment.md#re-embedding).

## Stores

The store interface is semantic (`UpsertDocument`, `HybridSearch`), not
SQL. Providers register by name. SQLite and postgres ship; OpenSearch
and S3 Vectors can register the same way and must pass
`internal/store/storetest`.

**sqlite** — FTS5 for the keyword leg, embeddings as BLOBs, brute-force
cosine in pure Go, RRF in Go. One file per KB. Zero dependencies.
Default.

**postgres** — tsvector full-text, pgvector for the dense leg (HNSW when
the server can build it, sequential scan otherwise), RRF in Go. One
schema per KB on a shared database. The DSN is `storage.options.dsn`.
Dimensions above 16000 are rejected: that is pgvector's cap.

Both refuse to open when the recorded identity disagrees with config.

## Inference readiness

Local inference servers take minutes to load models after a reboot.
Indexing must not start before the endpoint truly answers.

- `Embedder.WaitReady` sends a real 1-text embedding until dimensions
  match or the timeout expires (`--wait`, default 10m).
- `Chat.WaitReady` sends a one-token completion and **ignores the
  body**. A thinking-first model spends that token on reasoning and
  returns empty content, which is an index-time error but still proves
  the model is loaded.

Embeddings and chat are different models on a llama.cpp router. Waiting
only for embeddings lets the initial sync start against a chat endpoint
still answering `503 Loading model` or `400 model is not loaded`.

## Process shape

`andino-kb serve` is one process, one port:

- `/v1/*` — REST ([API](api.md))
- `/mcp` — MCP, stateless streamable HTTP, tool set built per request
  from the caller's scope
- `/healthz`, `/readyz`, `/metrics` — [ops](deployment.md#ops-endpoints)

`andino-kb index` is the same pipeline without the HTTP server or the
watchers: wait, sync, print document/chunk counts, exit.

`andino-kb search` opens the store and runs one hybrid query on stdout.
It does not wait for backends beyond whatever `app.New` needs to open
the store — the query embedding still hits the live endpoint.
