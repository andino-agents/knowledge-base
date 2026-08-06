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
  for why they can't.
- **Failure modes are the spec.** No silent fallbacks: an error must
  surface, never degrade data (see the design notes in the README for the
  history behind this).

## Dev setup

```bash
git clone https://github.com/andino-agents/knowledge-base
cd knowledge-base
go build ./...        # Go >= 1.26, no CGO, no external services
go test ./...         # tests need nothing but Go (SQLite runs in-process)
make build            # static binary in bin/andino-kb
```

Format with `gofmt` (CI rejects unformatted code) and run `go vet ./...`.

## Pull requests

- One concern per PR. Small is beautiful.
- New behavior needs tests. Storage providers must pass the conformance
  harness in `internal/store/storetest`.
- Commit messages: what changed and *why*, in the imperative. Reference
  issues (`Refs #N` / `Closes #N`).
- Breaking config changes need a migration note in the PR description.

## Bugs and features

Open an issue with the template. For retrieval-quality reports, include the
query, what you expected in the top results, and (if possible) `andino-kb
eval` output — that turns an anecdote into a testcase.

## Security

Found a vulnerability? Please do not open a public issue; email the
maintainer (see the org profile) and allow a reasonable window for a fix.

## License

By contributing you agree your contributions are licensed under
[Apache-2.0](LICENSE) (see section 5 of the license).
