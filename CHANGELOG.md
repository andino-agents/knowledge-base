# Changelog

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
