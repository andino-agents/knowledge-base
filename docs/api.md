# API

Base URL: `http://{bind}` (default `http://127.0.0.1:8180`). REST lives
under `/v1/`. MCP is streamable HTTP at `/mcp`. Both share the same
bearer keys and the same scopes.

## Authentication

Without `server.api_keys`, every endpoint and every MCP tool answers any
caller that can reach the port. That is the loopback default, and the
wrong one the moment you change `bind`.

With keys configured, send `Authorization: Bearer <key>`. Comparison is
constant-time. A missing or unknown key is `401`; a `read` key on a write
route is `403`.

| | `read` | `readwrite` |
|---|---|---|
| `GET /v1/kb...`, `POST .../search`, `POST /v1/search` | yes | yes |
| `POST .../documents`, `DELETE .../documents/{id}`, `POST .../reindex` | 403 | yes |
| MCP `search`, `get_document`, `list_*` | yes | yes |
| MCP `store`, `delete_document` | not advertised | yes |

A KB's `writable: true` is a separate axis: a `readwrite` key still
cannot write to a KB that is not writable.

`/healthz` is always open. `/readyz` always answers the plain `ready`
boolean. `/metrics` and the per-KB detail of `/readyz` follow
`server.ops_require_auth` (on whenever `api_keys` is set). See
[Deployment](deployment.md#ops-endpoints).

There is no TLS and no per-client identity. Terminate TLS in a reverse
proxy if the traffic leaves the host.

## REST

Errors are `{"error": "..."}` with the status in the HTTP code.

### `GET /v1/kb`

List knowledge bases.

```json
{
  "knowledge_bases": [
    {
      "name": "notes",
      "description": "Personal vault",
      "writable": false,
      "documents": 238,
      "chunks": 4102,
      "last_indexed_at": 1770000000
    }
  ]
}
```

### `GET /v1/kb/{kb}`

One KB. `404` if the name is unknown.

### `POST /v1/kb/{kb}/search`

```json
{
  "query": "how do we rotate the API keys",
  "limit": 8,
  "min_score": 0,
  "rerank": true,
  "max_per_doc": 2,
  "filter": { "team": "platform" }
}
```

| Field | Default | Notes |
|---|---|---|
| `query` | required | Non-empty. |
| `limit` | `8` | |
| `min_score` | `0` | Normalized relevance 0..1. After rerank this is the cross-encoder score; otherwise fused RRF scaled into `[0,1]`. |
| `rerank` | KB `rerank_default` | `null` = KB default. `false` disables for this request. `true` is a no-op if the KB has no reranker. |
| `max_per_doc` | `2` | Caps chunks per document, applied **after** reranking. Negative = no cap. |
| `filter` | none | Metadata equality, AND. |

```json
{
  "results": [
    {
      "document_id": 12,
      "knowledge_base": "notes",
      "source": "vault",
      "rel_path": "ops/keys.md",
      "uri": "file:///home/user/vault/ops/keys.md",
      "title": "API keys",
      "heading_path": "Rotation",
      "start_line": 40,
      "end_line": 72,
      "text": "...",
      "context": "...",
      "metadata": { "team": "platform" },
      "score": 0.031,
      "relevance": 0.94,
      "fts_rank": 1,
      "vec_rank": 2,
      "rerank_score": 0.94
    }
  ],
  "duration_ms": 26
}
```

`rerank_score` is present only when the cross-encoder ordered the
results. `fts_rank` / `vec_rank` are 1-based ranks in each retrieval
leg, `0` if that leg missed the chunk.

### `POST /v1/search`

Same body, across every KB. Results are tagged with `knowledge_base`.

### `GET /v1/kb/{kb}/documents`

Query: `prefix`, `cursor` (last `rel_path` of the previous page),
`limit` (default 50). Response: `{"documents": [...], "next_cursor": "..."}`.

### `GET /v1/kb/{kb}/document`

Query: `path` (required), `source` (optional; empty searches all sources
of the KB), `start_line`, `end_line`. Response:
`{"document": {...}, "content": "..."}`.

### `POST /v1/kb/{kb}/documents`

`readwrite`. Body: `{"content": "...", "title": "...", "metadata": {...}}`.
`201` with `{"document_id": "memory_20260904_101530_ab12cd34"}`. The id
follows the Strands memory convention. The KB must be `writable`.

### `DELETE /v1/kb/{kb}/documents/{id}`

`readwrite`. Deletes a managed document by the id `store` returned.
`404` if it is not there.

### `POST /v1/kb/{kb}/reindex`

`readwrite`. Starts a background sync of every source. `202` with a job
object. The job is in-process memory: a restart forgets it.

```json
{
  "id": "1",
  "knowledge_base": "notes",
  "status": "running",
  "started_at": "2026-09-04T10:15:30Z"
}
```

`status` is `running` \| `done` \| `failed`. Poll
`GET /v1/kb/{kb}/reindex/{id}`.

This is an incremental sync, not `--rebuild`. It does not drop the
store.

## MCP

Stateless streamable HTTP at `/mcp`. The tool set is built **per
request** from the caller's scope: a `read` key never sees `store` or
`delete_document` in `tools/list`.

| Tool | Arguments | Returns |
|---|---|---|
| `search` | `query` (required); `knowledge_base`, `limit`, `min_score`, `rerank`, `max_per_doc`, `filter` | `{results: [...]}` — same hit shape as REST, including `rerank_score` |
| `list_knowledge_bases` | none | `{knowledge_bases: [{name, description, writable, documents, chunks}]}` |
| `list_documents` | `knowledge_base`; `prefix`, `cursor`, `limit` | `{documents, next_cursor}` |
| `get_document` | `knowledge_base`, `path`; `start_line`, `end_line` | `{document, content}` |
| `store` | `knowledge_base`, `content`; `title`, `metadata` | `{document_id, status: "stored"}` |
| `delete_document` | `knowledge_base`, `document_id` | `{status: "deleted"}` |

`search` first, then `get_document` with the hit's `rel_path` and line
span to expand context. Writable KBs show `writable: true` on
`list_knowledge_bases`.
