# Retrieval & Evals

## How a query runs

1. Embed the query (same model and dimensions as the KB).
2. Hybrid search in the store: keyword (FTS5 / tsvector) + dense
   (in-memory cosine on SQLite, pgvector on postgres).
3. Fuse with Reciprocal Rank Fusion (`k = 60`).
4. Apply metadata `filter` if the request sent one.
5. Optionally rerank the top 24 fused candidates with a cross-encoder.
6. Cap chunks per document (`max_per_doc`, default 2) — **after**
   reranking, never before. Applying it earlier measurably lost answers.
7. Truncate to `limit` and drop hits below `min_score`.

Keyword-only misses paraphrases; vector-only misses exact identifiers.
You want both. Contextual retrieval (below) is the lever that moved
recall@1 on a real corpus; reranking is the expensive one.

A rerank failure **degrades to fusion order** and logs a warning. That
is the one silent fallback in the search path: embeddings have none. An
embedding error fails the query.

SQLite vector search is a brute-force cosine scan held in memory
(embeddings persist as BLOBs). At the supported scale — up to ~100k
chunks per KB — that is single-digit milliseconds in pure Go. The store
benchmark (`BenchmarkHybridSearch`) is **14 ms over 10k chunks × 1024
dims** without query embedding; end-to-end with a local llama.cpp is
about 30 ms. Switch the storage provider to **postgres** when that
scale no longer fits.

## Contextual retrieval

Per-KB opt-in. At index time a chat model writes a short situating
context (1–2 sentences, same language as the document) for every chunk
of a multi-chunk document. The context is embedded and BM25-indexed
**with** the text.

Single-chunk documents are skipped: the chunk *is* the document, and a
generated context adds noise. Agent `store` / managed writes skip
contextual retrieval entirely — they are treated as markdown and
embedded as-is. Prompts put the document first and identically for
every chunk of the same file, so a prefix-caching server pays the
document once. Four workers generate contexts in parallel.
Documents longer than ~8000 estimated tokens are truncated **in the
prompt only**; the chunks themselves are never truncated.

Empty chat content is a hard error (typically a thinking-first model
that spent `max_tokens` on reasoning). Set
`extra_body.chat_template_kwargs.enable_thinking: false`. See
[Configuration](configuration.md#chat-models).

On the measured corpus, contextual retrieval made reranking net-neutral
at ~30× the latency, so `rerank_default: "off"` is the usual setting
once contextual is on. The reranker stays available per request.

## Measured numbers

Real 238-document knowledge base, 40 golden queries, document-level
metrics, local llama.cpp inference:

| Configuration | recall@1 | recall@5 | MRR | p50 latency |
|---|---|---|---|---|
| Hybrid (BM25 + vectors + RRF) | 75% | 98% | 0.848 | 24 ms |
| + cross-encoder rerank | 82% | 98% | 0.884 | ~850 ms |
| + **contextual retrieval** (no rerank) | **88%** | **98%** | **0.908** | **26 ms** |

A change that touches retrieval quality goes with numbers from
`andino-kb eval` on a real corpus, or a clear reason why it cannot be
measured.

## `andino-kb eval`

Talks to a **running** `serve`. Document-level metrics: recall@1,
recall@N, MRR@N, latency p50/p95. Neither `eval` nor `parity` sends an
`Authorization` header: against a server with `api_keys` they get 401.
Run them on loopback without keys, or temporarily open the port.

```bash
andino-kb serve -c config.yaml

andino-kb eval --queries parity/queries.yaml --kb notes --top 5

# A/B a search knob
andino-kb eval --queries parity/queries.yaml --kb notes \
  --a '{"rerank":false}' --b '{"rerank":true}' --json /tmp/eval.json
```

| Flag | Default | Notes |
|---|---|---|
| `--queries` | `parity/queries.yaml` | Golden set (see below). |
| `--url` | `http://127.0.0.1:8180` | |
| `--kb` | all KBs (`POST /v1/search`) | |
| `--top` | `5` | Documents considered per query. |
| `--json` | none | Write the full run (or A/B pair) to this file. |
| `--a` / `--b` | none | JSON search-body overrides. `--b` enables compare mode and per-query deltas. |

A query counts as a hit when any returned `rel_path` **contains** any
`expect` substring. Rank is the first matching document (1-based);
`0` is a miss.

```yaml
queries:
  - query: "how do I force a full reindex"
    expect: ["operations/runbook"]
  - query: "what did we decide about the API gateway"
    expect: ["decisions/adr-012", "decisions/adr-014"]
```

Keep your own set out of public repos if the paths reveal private
content — `--queries` can point anywhere.
[parity/queries.example.yaml](../parity/queries.example.yaml) is the
shape.

## `andino-kb parity`

Same golden set, two systems. The gate passes when andino recall >=
legacy recall. The legacy side speaks the vault-mcp REST shape
(`POST /query`, results or sources carrying `file_path`).

```bash
andino-kb parity \
  --queries parity/queries.yaml \
  --andino-url http://127.0.0.1:8180 \
  --legacy-url http://127.0.0.1:8000 \
  --kb notes --top 5
```

The default `--legacy-url` is `http://127.0.0.1:8000`. A missing legacy
server prints `err` on that side and counts as a miss, so the gate is
easy to pass by accident — only trust a run where the legacy column
shows `HIT`/`miss`, not `err`.
