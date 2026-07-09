# Contributing to Herd Auto Prompter

Thanks for helping keep herds unblocked! This guide covers the essentials.

## Ground rules

- **Conventional Commits.** Commit messages follow
  [Conventional Commits](https://www.conventionalcommits.org/):
  `feat: ...`, `fix: ...`, `docs: ...`, `test: ...`, `refactor: ...`,
  `chore: ...`. Breaking changes use `!` (e.g. `feat!: ...`).
- **SemVer.** Releases are tagged `vMAJOR.MINOR.PATCH`; the release workflow
  builds artifacts from the tag. `min_herdr_version` in `herdr-plugin.toml`
  is bumped only deliberately when new Herdr APIs are adopted.
- **The domain core stays pure.** `internal/domain` must not import Herdr,
  SQLite, LLM, or any adapter package — `TestDomainPurity` enforces this.
  Side effects live behind the ports in `internal/ports`.
- **Fail-safe on the daemon path.** No panics; every error path resolves to
  escalate + log. New adapter calls run under `logging.Guard`.
- **Safety tests are non-negotiable.** Changes touching the allowlist, kill
  switch, confidence gate, rate guard, or retry ceiling must keep (and where
  relevant, extend) the safety-invariant tests. New irreversible-operation
  shapes belong in `internal/domain/testdata/irreversible_corpus.txt` — CI
  fails if the seed patterns miss any corpus entry.

## Build & test

```sh
go build ./...                 # full build (pure Go, CGO not required)
go test ./... -count=1         # full suite: unit, golden, safety, concurrency, integration
gofmt -l . && go vet ./...     # what CI gates on
golangci-lint run              # lint (CI runs this too)
```

Golden classifier fixtures live in `internal/classify/testdata/`; regenerate
expectations with `UPDATE_GOLDEN=1 go test ./internal/classify/` and review
the diff carefully.

To exercise your working tree inside Herdr:

```sh
go build -o bin/herd-auto-prompter ./cmd/herd-auto-prompter
herdr plugin link .
```

## Pull requests

1. Fork/branch from `main`.
2. Keep PRs focused; include tests for behavior changes.
3. Make sure `go test ./...`, `gofmt`, `go vet`, and `golangci-lint` pass —
   CI gates on all of them plus the allowlist-corpus regression.
4. Describe *what* and *why* in the PR body; link related issues.

## Release flow (maintainers)

1. Update `version` in `herdr-plugin.toml`.
2. Tag: `git tag vX.Y.Z && git push origin vX.Y.Z`.
3. The Release workflow runs the full CI gate, builds Linux/macOS binaries,
   and publishes a GitHub Release — after which
   `herdr plugin install 0xGosu/herdr-auto-pilot --ref vX.Y.Z` resolves the
   pinned tag.
