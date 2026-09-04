# Contributing to andino-kb

Thanks for your interest. This is a small, focused project; contributions
that fit its philosophy are very welcome.

## The philosophy (read this first)

- **Headless and agent-first.** No web UI. The MCP/REST surface is the
  product. Tool descriptions are written for agents, not humans.
- **One static binary.** `CGO_ENABLED=0` is non-negotiable; a dependency
  that breaks pure-Go cross-compilation will not be merged.
- **Measured, not vibed.** Retrieval changes must come with numbers from
  `andino-kb eval` (recall@k, MRR) on a real corpus, or a clear reasoning
  for why they can't. See [Retrieval & Evals](docs/evals.md).
- **Failure modes are the spec.** No silent fallbacks: an error must
  surface, never degrade data. The one documented exception is a rerank
  failure, which falls back to fusion and logs a warning.

Product behaviour is described in [docs/](docs/). A PR that changes
behaviour updates the matching page in the same change.

## Dev setup

```bash
git clone https://github.com/andino-agents/knowledge-base
cd knowledge-base
go build ./...        # Go >= 1.26, no CGO, no external services
go test ./...         # SQLite runs in-process; pgvector tests skip without Docker
make build            # static binary in bin/andino-kb
andino-kb doctor -c config.yaml
```

Format with `gofmt` (CI rejects unformatted code) and run `go vet ./...`.

## Pull requests

- One concern per PR. Small is beautiful.
- New behavior needs tests. Storage providers must pass the conformance
  harness in `internal/store/storetest`.
- Commit messages: conventional commits in English, imperative, with the
  *why* in the body. Reference issues (`Refs #N` / `Closes #N`).
- Breaking config changes need a migration note in the PR description
  and a changelog entry.

## Bugs and features

Open an issue with the template. For retrieval-quality reports, include the
query, what you expected in the top results, and (if possible) `andino-kb
eval` output — that turns an anecdote into a testcase.

## Security

Found a vulnerability? Please do not open a public issue; see
[SECURITY.md](SECURITY.md).

## License

By contributing you agree your contributions are licensed under
[Apache-2.0](LICENSE) (see section 5 of the license).
