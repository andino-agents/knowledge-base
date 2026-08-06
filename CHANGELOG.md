# Changelog

## v0.2.0

Retrieval quality, measured. On a real 238-document KB with 40 golden
queries: recall@1 75% -> 88%, MRR 0.848 -> 0.908 at 26 ms p50.

- **Contextual retrieval** (per-KB opt-in): a chat model situates every
  chunk within its document at index time; the context is embedded and
  BM25-indexed with the text. Parallel generation (4 workers), prefix-cache
  friendly prompts, single-chunk documents skipped. Migration 0002 upgrades
  existing databases in place.
- **`andino-kb eval`**: recall@1/@N, MRR, latency percentiles over a golden
  query set, with `--json` runs and `--a/--b` config comparison. This
  harness gated every change in this release.
- **Result diversification**: `max_per_doc` (default 2) caps chunks per
  document, applied after reranking (applying it before measurably lost
  answers). Rerank candidate pool bounded at 24 (same recall as 80, a third
  of the latency).
- **Per-request `rerank` override and per-KB `rerank_default`**: on the
  measured corpus, contextual retrieval made reranking net-neutral at 30x
  the latency, so it can now default off while staying available per query.
- **`chat_models` with `extra_body`**: provider-specific knobs such as
  `chat_template_kwargs: {enable_thinking: false}` for thinking-first
  models, whose empty responses are now hard errors instead of silently
  stored empty contexts.

## v0.1.0

Initial release.

- Hybrid retrieval: FTS5 (BM25) + in-memory cosine KNN over SQLite-persisted
  embeddings, fused with Reciprocal Rank Fusion; optional cross-encoder
  reranking via any OpenAI-compatible `/v1/rerank`.
- Sources: local directory (recursive watcher, atomic-write safe, per-path
  debounce) and git repository (pure-Go shallow clone + poll).
- Writable knowledge bases: agents `store`/`delete` documents (Strands
  memory-tool compatible semantics and ids).
- Interfaces: MCP over stateless streamable HTTP (official Go SDK), REST
  with bearer keys scoped read/readwrite, `/healthz` `/readyz` `/metrics`.
- Incremental sync driven by a content-hash manifest, with an empty-index
  guard; embedding failures abort documents, never degrade them.
- Storage behind a provider interface; SQLite (one file per KB) ships,
  pgvector/OpenSearch/S3 Vectors can register later.
- `andino-kb parity`: golden-query recall harness to gate migrations from an
  existing RAG.
- Single static binary (`CGO_ENABLED=0`), linux/darwin × amd64/arm64.
