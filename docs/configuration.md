# Configuration

One YAML file: server, storage, inference backends, defaults, knowledge
bases. See [config.example.yaml](../config.example.yaml) for the annotated
template.

Decoding is **strict**: unknown fields are errors. `${VAR}` expands from
the environment before parsing; unset variables expand to `""` and fail
validation where a value is required (an empty API key is a startup
error, not a quietly disabled key).

```bash
andino-kb validate -c config.yaml
```

## `server`

| Field | Default | Notes |
|---|---|---|
| `bind` | `127.0.0.1:8180` | Listen address. Loopback is the assumed threat model. |
| `data_dir` | required | SQLite files, git clones (`data_dir/git/<kb>-<source>`). |
| `api_keys` | none | Omit entirely for unauthenticated localhost use. |
| `api_keys[].key` | required if present | Expand from the environment. |
| `api_keys[].scope` | `readwrite` | `read` or `readwrite`. |
| `ops_require_auth` | follow `api_keys` | Gates `/metrics` and the per-KB detail of `/readyz`. `/healthz` stays open. |
| `log_level` | `info` | `debug` \| `info` \| `warn` \| `error` |
| `log_format` | `text` | `text` \| `json` |

```yaml
server:
  bind: "127.0.0.1:8180"
  data_dir: "/var/lib/andino-kb"
  api_keys:
    - key: "${ANDINO_KB_READ_KEY}"
      scope: read
    - key: "${ANDINO_KB_ADMIN_KEY}"
      scope: readwrite
  # ops_require_auth: true
  log_level: info
  log_format: text
```

Scopes apply to REST and MCP equally. See [Deployment](deployment.md#security).

## `storage`

| Field | Default | Notes |
|---|---|---|
| `provider` | `sqlite` | `sqlite` or `postgres`. |
| `options` | none | Provider-specific map. Unknown provider names fail at `store.Open`. |

**sqlite** (default): one file per KB at `<data_dir>/<kb>.db`. No options.

**postgres**: pgvector. `options.dsn` is required — a `postgres://`
connection string. Each KB is isolated in its own schema (the DSN is
rewritten with `search_path=<kb>,public` on open). Several KBs share one
database without colliding.

```yaml
storage:
  provider: postgres
  options:
    dsn: "postgres://user:pass@localhost:5432/andino_kb?sslmode=disable"
```

A top-level `storage.dsn` is an unknown field and a startup error. The
DSN lives under `options`.

## `inference`

Two backend types. `openai`, the default, is any OpenAI-compatible endpoint:
llama.cpp, vLLM, Ollama, OpenAI. `bedrock` calls Amazon Bedrock directly
through the AWS SDK, with no proxy in between.

```yaml
inference:
  backends:
    - name: local-llama
      base_url: "http://127.0.0.1:8080/v1"
      api_key: "${LLAMA_API_KEY}"
  embedding_models:
    - name: qwen3-embed
      backend: local-llama
      model: "qwen3-embedding-0.6b"
      dimensions: 1024          # required; a mismatch with the store is a hard error
      batch_size: 32            # default 32
      max_retries: 4            # default 4; only 5xx, 429 and network
  rerank_models: []             # optional /v1/rerank
  chat_models: []               # contextual retrieval and OCR
```

### Backends

| Field | Notes |
|---|---|
| `name` | Required. Models refer to it. |
| `type` | `openai` (default) or `bedrock`. |
| `base_url`, `api_key` | `openai` only, and `base_url` is required there. |
| `region` | `bedrock` only, and required there. |

A `bedrock` backend has no key of its own: credentials come from the AWS
default chain (environment, shared profile, or the instance role on a VM).
Setting `base_url` or `api_key` on it is a load error rather than a silently
ignored field.

```yaml
inference:
  backends:
    - name: aws
      type: bedrock
      region: us-east-1
  embedding_models:
    - name: titan
      backend: aws
      model: amazon.titan-embed-text-v2:0
      dimensions: 1024          # Titan v2 serves 256, 512 or 1024
  chat_models:
    - name: summary
      backend: aws
      model: us.anthropic.claude-haiku-4-5-20251001-v1:0
      max_tokens: 200
```

What Bedrock supports, and what it does not:

- **Embeddings: Titan Text Embeddings v2 only.** It takes the output size
  as a request field, so `dimensions` is honoured rather than assumed. Other
  Bedrock embedding models are refused at the first call with a clear error.
  Titan embeds one text per request, so `batch_size` has no effect.
- **Chat: any model the Converse API serves**, including vision models for
  OCR (png, jpeg, gif and webp). `extra_body` travels as
  `additionalModelRequestFields`. No `temperature` is sent: recent Anthropic
  models on Bedrock reject it.
- **Reranking: not supported.** A rerank model on a `bedrock` backend is a
  load error.
- The IAM principal needs `bedrock:InvokeModel` on the embedding model and
  `bedrock:Converse` (granted by `bedrock:InvokeModel`) on the chat model,
  plus model access enabled for both in the account.

Retries follow the same rule as HTTP: throttling and server-side errors
retry with backoff, validation and permission errors fail at once. The SDK's
own retries are off, so attempts are not multiplied.

### Embedding models

`name` + `model` + `backend` + `dimensions` are required. `dimensions`
must match what the endpoint actually returns; a wrong model behind the
name is a hard error, not a silent reshape.

### Rerank models

`name` + `model` + `backend`. The client calls `/v1/rerank`. Community
Qwen3-Reranker GGUFs often need `--reranking --pooling rank` and a
`cls.output.weight`; `doctor` catches degenerate scores.

### Chat models

Used at index time for contextual retrieval and OCR, not for serving
answers.

| Field | Default | Notes |
|---|---|---|
| `name`, `model`, `backend` | required | |
| `max_tokens` | `200` | |
| `extra_body` | none | Merged into `/v1/chat/completions`; on Bedrock, sent as `additionalModelRequestFields`. |

Thinking-first models spend `max_tokens` on reasoning and return empty
content, which is a hard error at index time. Disable thinking:

```yaml
chat_models:
  - name: local-chat
    backend: local-llama
    model: "qwen3.6-35b-a3b"
    max_tokens: 200
    extra_body:
      chat_template_kwargs: { enable_thinking: false }
```

Embeddings and chat load as **separate** models on a llama.cpp router.
`serve` and `index` wait for both (`WaitReady`) before indexing. Do not
remove that wait.

## `defaults`

Applied to every knowledge base that does not set the field itself.

| Field | Default |
|---|---|
| `chunking.strategy` | accepted (`markdown` \| `plaintext` \| `code`), **unused** — the extractor is chosen by file extension, not this field |
| `chunking.max_tokens` | `512` (chars/4 heuristic) |
| `chunking.overlap_tokens` | `64` |
| `embedding_model` | required by each KB if unset here |
| `rerank_model` | `""` (disabled) |
| `ocr` | none |

## `knowledge_bases`

At least one is required. A KB needs at least one source **or**
`writable: true`.

| Field | Notes |
|---|---|
| `name` | Unique. Becomes the SQLite filename / postgres schema. |
| `description` | Returned by `list_knowledge_bases` / `GET /v1/kb`. |
| `writable` | Agents may `store` / `delete` here. Source name `managed` is reserved. |
| `sources` | See below. |
| `chunking` | Overrides `defaults.chunking`. |
| `embedding_model` | Ref into `inference.embedding_models`. |
| `rerank_model` | Ref into `inference.rerank_models`. Empty = no reranker. |
| `rerank_default` | `on` (default) or `off`. With `off` the reranker stays available per request (`rerank: true`). |
| `contextual` | `{enabled, model}` — model is a `chat_models` name. Applies to source sync, not to agent `store`. |
| `ocr` | `{enabled, model}` — vision-capable `chat_models` name. Without it, scanned PDF pages are skipped. |

```yaml
knowledge_bases:
  - name: team-docs
    description: "Engineering docs"
    contextual:
      enabled: true
      model: local-chat
    rerank_default: "off"
    sources:
      - name: docs-repo
        type: git
        url: "https://github.com/example/docs.git"
        paths: ["docs/**/*.md"]
        poll_interval: 5m
        token_env: GH_TOKEN
  - name: agent-memory
    writable: true
```

## Sources

Type-specific fields are flat; the validator rejects fields that belong
to another type.

### `localdir`

| Field | Default | Notes |
|---|---|---|
| `path` | required | Absolute or relative directory. |
| `include` | all indexable extensions | Doublestar globs. |
| `exclude` | none | Applied after include. |
| `watch` | `false` | Recursive filesystem watcher. |
| `debounce_ms` | `2000` | Per-path debounce. The watcher marks dirty and re-stats; it does not branch on event types. |

```yaml
- name: vault
  type: localdir
  path: "/home/user/vault"
  include: ["**/*.md"]
  exclude: [".obsidian/**", ".trash/**"]
  watch: true
```

### `git`

Pure-Go shallow clone into `<data_dir>/git/<kb>-<source>`, then poll.

| Field | Default | Notes |
|---|---|---|
| `url` | required | |
| `branch` | `main` | |
| `paths` | all indexable files | Doublestar globs. |
| `poll_interval` | `5m` | |
| `token_env` | none | Name of an env var holding a token, not the token. |

```yaml
- name: docs-repo
  type: git
  url: "https://github.com/example/docs.git"
  branch: main
  paths: ["docs/**/*.md", "README.md"]
  poll_interval: 5m
  token_env: GH_TOKEN
```

### `s3`

Paginated listing into the same incremental sync. Credentials come from
the standard AWS chain (env, shared config, SSO, IRSA, instance
profiles). Nothing secret goes in this file.

| Field | Default | Notes |
|---|---|---|
| `bucket` | required | |
| `prefix` | none | Stripped from `rel_path`. |
| `paths` | all indexable keys | Doublestar globs over the stripped key. |
| `poll_interval` | `5m` | |
| `region` | chain default | |
| `endpoint` | none | MinIO / S3-compatible. |
| `path_style` | `false` | Required by most MinIO setups. |

```yaml
- name: bucket
  type: s3
  bucket: "corp-documents"
  prefix: "policies/"
  paths: ["**/*.pdf", "**/*.docx"]
  poll_interval: 10m
  endpoint: "http://minio.internal:9000"
  path_style: true
```

## What gets indexed

Only extensions the extractor registry knows enter a manifest. Everything
else is invisible to sync.

| Extractor | Extensions |
|---|---|
| Markdown | `.md`, `.markdown` — YAML frontmatter skipped, ATX headings build the heading path, fences kept verbatim |
| Plaintext | `.txt`, `.rst`, `.adoc` |
| Code | `.go` `.py` `.js` `.ts` `.tsx` `.jsx` `.java` `.rb` `.rs` `.c` `.h` `.cpp` `.hpp` `.cs` `.sh` `.bash` `.sql` `.tf` `.yaml` `.yml` `.toml` `.json` `.proto` |
| PDF | `.pdf` — per-page blocks; scanned pages go through OCR if enabled |
| DOCX | `.docx` — Word heading styles feed the heading path |

Chunking never merges across heading-path boundaries. Oversized blocks
split by lines with `overlap_tokens` of trailing context.
`chunking.strategy` is accepted so existing files keep loading; it does
not pick an extractor.
