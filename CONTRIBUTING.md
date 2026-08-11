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
- **Safety tests are non-negotiable.** Changes touching the never-auto patterns, kill
  switch, confidence gate, rate guard, or retry ceiling must keep (and where
  relevant, extend) the safety-invariant tests. New irreversible-operation
  shapes belong in `internal/domain/testdata/irreversible_corpus.txt` — CI
  fails if the seed patterns miss any corpus entry.

## Build & test

The semantic signature matcher links native code (llama.cpp via CGO and
FAISS behind bleve's `vectors` build tag), so builds need a C/C++ toolchain,
cmake, a one-time native-deps build, and the `vectors cpu` build tags:

```sh
bash scripts/setup-native.sh                 # one-time: submodules, llama-go libs, FAISS
go build -tags "vectors cpu" ./...           # full build (CGO)
go test -tags "vectors cpu" ./... -count=1   # full suite: unit, golden, safety, concurrency, semantic
gofmt -l . | grep -v submodule ; go vet -tags "vectors cpu" ./...
golangci-lint run --build-tags "vectors,cpu" # lint (CI runs this too)
```

Both tags are always required — a build without them fails to link, so there is
no tag-free shortcut for a quick check.

Never `git add` the llama-go submodule directory after pointing it at another
checkout with a symlink (a common local trick for reusing prebuilt native
archives): that rewrites its tree entry from a gitlink to a symlink, and a
fresh clone then fails inside `setup-native.sh` with a linker error that names
neither git nor submodules. `make check` and every CI job that builds native
deps run `bash scripts/check-submodule-gitlink.sh` first to catch it. In a
worktree, `git config submodule.<name>.ignore all` (the `<name>` from the
`[submodule "…"]` header — it equals the path here) keeps `git add -A` off it,
but an explicit `git add <that path>` still records the symlink, so never run
one.

Golden classifier fixtures live in `internal/classify/testdata/`; regenerate
expectations with `UPDATE_GOLDEN=1 go test -tags "vectors cpu" ./internal/classify/`
and review the diff carefully.

To exercise your working tree inside Herdr:

```sh
go build -tags "vectors cpu" -o bin/hap ./cmd/hap
herdr plugin link .
```

## Pull requests

1. Fork/branch from `main`.
2. Keep PRs focused; include tests for behavior changes.
3. Make sure the tagged commands above pass — `go test -tags "vectors cpu" ./...`,
   `gofmt`, `go vet`, and `golangci-lint`. CI gates on all of them, plus:
   - the never-auto patterns-corpus regression (`corpus-gate`);
   - a **race-detector** job over `internal/{store,domain,control,embedder,match,daemon}`,
     retried up to 3 times because the detector is flaky under CI load;
   - a **macOS** matrix leg, where test binaries need `-ldflags "-r /usr/local/lib"`
     to reach the FAISS dylibs.
4. Describe *what* and *why* in the PR body; link related issues.
5. **Add an entry to `CHANGELOG.md`** — required for every change, including
   patch-level fixes, in the same commit as the change itself. There is no
   `Unreleased` heading: merging auto-releases the next patch, so head your
   section with that number. Read the file on the latest remote `main`, take the
   topmost `## X.Y.Z`, and use `X.Y.(Z+1)` if that version is already tagged —
   or append to it if it is not (another unmerged PR opened it). A deliberate
   minor/major bump uses the version you set in `herdr-plugin.toml` instead.
   Verb-first one-liners (`Added …`, `Fixed …`); write what it means for the
   reader, not what the diff did. GitHub already generates a list of PR titles
   per release; that file is for what a title cannot convey, like the bounds of
   a new action. See the Changelog section in `CLAUDE.md`.

Never put `[skip ci]`, `[ci skip]`, or `[no ci]` anywhere in the squash-merge
message (title or body) of a PR that should release. GitHub suppresses *all*
workflows for a ref whose head commit carries one — including the release tag
push onto that commit — so the release silently never builds.

## Release flow (maintainers)

Releases are **automated on merge to `main`** (`.github/workflows/auto-release.yml`).
`version` in `herdr-plugin.toml` is the single source of truth, and it always
names a version whose GitHub release already exists — it *trails* releases,
never leads them.

- **Patch (the default): do nothing.** Merge the feature PR. The workflow sees
  the manifest version already tagged, computes the next patch, squash-merges a
  bump PR (`release/bump-vX.Y.Z+1`, commit marked `[skip release]`), and tags
  that bump commit — which fires the tag-driven `release.yml`. Do not bump the
  manifest by hand for patch work.
- **Minor/major (the reserved manual path):** overwrite `version` in
  `herdr-plugin.toml` *inside* your feature PR (e.g. `0.6.0`). On merge the
  workflow finds that version untagged, skips the bump, and tags the merge
  commit directly.

`release.yml` then runs the full CI gate and builds on three native runners
(CGO cannot cross-compile), publishing the platform binaries, the per-platform
native tarballs, the embedding model, and `SHA256SUMS`.

The manifest version and the tag MUST match: `herdr plugin install` runs
`scripts/install.sh`, which downloads the release asset for the version
declared in `herdr-plugin.toml` (that's what removes the Go-toolchain
dependency for users). The automation preserves this by construction. Between
the bump merge and the release publishing (~15 min), `main`'s manifest names a
version with no assets; rather than 404, `install.sh` falls back to the newest
earlier release that has them and says so loudly, and `hap update` picks up the
intended version once it publishes. A `--ref vX.Y.Z` pin avoids that path only
by naming a release that already has assets — install.sh sees the manifest, not
the git ref; `HAP_NO_FALLBACK` is the guaranteed refusal. If the release *build*
fails after the tag exists, re-run that `release.yml` run — never re-run
auto-release, which would advance versions.
