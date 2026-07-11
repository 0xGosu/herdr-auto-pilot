# CLAUDE.md

Herd Auto Prompter (**hap**) — a Go plugin for the herdr terminal multiplexer
that watches every agent pane, auto-answers when a learned rule is confident,
and escalates to the operator (or a local LLM CLI) when not.
`CONTRIBUTING.md` has the full ground rules; this file is the working
reference for day-to-day development.

## Build, test, lint

The semantic signature matcher links native code (llama.cpp via CGO,
FAISS behind bleve's `vectors` build tag), so **every build needs the
native deps once** and the `vectors cpu` build tags always:

```sh
bash scripts/setup-native.sh                   # one-time: submodules + llama-go libs + FAISS → /usr/local/lib
go build -tags "vectors cpu" ./...             # CGO; needs a C/C++ toolchain
go test -tags "vectors cpu" ./... -count=1     # full unit/golden/safety/semantic suite (what CI runs)
gofmt -l . | grep -v third_party && go vet -tags "vectors cpu" ./...
golangci-lint run --build-tags "vectors,cpu"   # CI runs this too
```

- `cpu` disables llama-go's default GPU (Vulkan/Metal) linkage; `vectors`
  turns on bleve's FAISS-backed KNN. Both are required — a build without
  them fails to link or compile.
- The real-model embedder test skips unless `models/all-minilm-l6-v2-q8_0.gguf`
  exists (download once from the HF repo in `release.yml`, or set
  `HAP_TEST_EMBED_MODEL`).
- Run the full suite before every commit that touches Go code.
- Golden classifier fixtures: `internal/classify/testdata/`; regenerate with
  `UPDATE_GOLDEN=1 go test ./internal/classify/` and review the diff.
- Try the working tree inside herdr: `go build -o bin/hap ./cmd/hap && herdr plugin link .`
- Full pipeline smoke test (fake herdr → real daemon → real LLM CLI):
  `go build -o /tmp/e2e ./e2e_harness && /tmp/e2e <short-dir> <hap-bin> <config-dir> <state-dir>`,
  then inspect with `hap audit` / replay `get_context` via `hap mcp`.

## Local integration suite (real herdr + claude)

`test/integration/` holds real-dependency tests that drive an **actual
running herdr** (and, when enabled, a **real Claude Code CLI**). They are
gated by the `integration` build tag, so `go test ./...` and CI never run
them — run them by hand:

```sh
# from inside a running herdr, or with HERDR_BIN_PATH set:
go test -tags integration ./test/integration/ -v

# also drive a real, authenticated claude (spends tokens):
HAP_ITEST_CLAUDE=1 go test -tags integration ./test/integration/ -v

# include the real-model semantic matching case (needs the native deps):
go test -tags "integration vectors cpu" ./test/integration/ -v
```

- Each test **skips** (never fails) when its dependency is absent, so the
  command is safe to run anywhere; it only asserts when the real tools are
  present.
- The suite loads `test/integration/testdata/config.toml` (the Claude Code
  recipe) — edit it to match the CLI you want to exercise.
- Current cases: `TestRealPaneInfo` (herdr `pane get` → cwd/ids),
  `TestRealConfirmDeliversMenuDigit` (confirming a label reply selects the
  numbered menu — the send-content regression), `TestRealClaudeConsult`
  (gated by `HAP_ITEST_CLAUDE=1`) drives a **real Claude Code session
  (`--model haiku`)** to a real approval menu, confirms it through the
  plugin, and asserts the menu digit reached claude (the command runs).
  `TestRealEmbeddingSemanticMatch` (needs the extra `vectors cpu` tags)
  drives a **real llama.cpp embedding model + FAISS index** through an
  in-process daemon: a rule learned for one approval auto-answers a
  paraphrase (cosine ≥ 0.90) and leaves an unrelated approval alone; skips
  when `models/all-minilm-l6-v2-q8_0.gguf` is absent
  (`HAP_TEST_EMBED_MODEL` overrides).
- The claude case skips (never fails) if it can't elicit a prompt; it needs a
  path OUTSIDE claude's auto-approved dirs (`/tmp`, `/workspaces`,
  `~/.claude`) to force the permission menu — it touches a `$HOME` dotfile.
  Override the model with `HAP_ITEST_CLAUDE_MODEL`.

**Recommended: run the integration suite once after finishing any feature**,
before opening the PR — the unit suite fakes herdr, so only this catches real
CLI-shape drift (e.g. `pane read --source recent` vs `visible`, `agent send`
delivering a menu digit vs a label).

## Commits

Format: `#<issue> <type>: <subject>` — a Conventional Commit prefixed with a
GitHub issue reference. A commit-msg hook **rejects messages that don't start
with a ticket/issue id**. Examples from history:

```
#1 feat: enrich LLM consult context — location ids, cwd, configurable pane excerpt
#1 fix: run daemon from state dir, self-heal stale daemons on upgrade
#1 chore: bump plugin version to 0.1.14
```

- Types: `feat`, `fix`, `docs`, `test`, `refactor`, `chore`; breaking → `feat!:`.
- Pre-commit hooks also check large files, secrets, trailing whitespace, and
  line endings — let them run (don't `--no-verify`).
- Never commit directly to `main`. Branch (`feat/…`, `fix/…`), open a PR.
- For any non-trivial change, prefer an isolated worktree
  (`worktree-agent-noN` beside the repo) so `main`'s checkout stays clean;
  remove the worktree and delete the branch (local + origin) after merge.

## Version bump & release

Releases are **automated on merge to main** (`.github/workflows/
auto-release.yml`), versioned from `herdr-plugin.toml`:

- **Patch (the default)** — merge an ordinary PR: the workflow sees the
  manifest version is already tagged, opens a `release/bump-vX.Y.Z+1` PR
  (its commit carries `[skip ci]`), squash-merges it with the owner's
  `RELEASE_PAT` secret, tags, and runs the release build via workflow_call.
  Do NOT bump the manifest for patch-level work.
- **Minor/major (the reserved manual path)** — bump `version` in
  `herdr-plugin.toml` INSIDE your feature PR; on merge the workflow sees an
  untagged manifest version and releases exactly it (no bump PR).
- Doc/workflow-only pushes (`**.md`, `docs/**`, `.github/**`) and merge
  commits containing `[skip release]` do not release. Hand-pushing a
  `v*.*.*` tag still works (release.yml keeps its tag trigger) for
  emergencies/re-releases.

The release build runs the full CI gate, then builds on THREE native
runners (CGO cannot cross-compile; Intel macOS is deliberately
unsupported): `hap-{linux-amd64,linux-arm64,darwin-arm64}` (llama.cpp
statically linked in), a `hap-native-<os>-<arch>.tar.gz` per platform
(FAISS shared libs, plus libomp on macOS, rpath'd to `<plugin>/lib`), the
`all-minilm-l6-v2-q8_0.gguf` embedding model fetched from Hugging Face
(sha256-pinned), and `SHA256SUMS`; then publishes the GitHub Release.
`install.sh` treats the binary and native tarball as REQUIRED and the model
as optional (BM25 fallback).

The invariant: **`version` in `herdr-plugin.toml` and the git tag MUST
match** — the automation preserves it by construction. `scripts/install.sh`
downloads the release asset named by the manifest version.

Verify after any release: `gh release view vX.Y.Z` — expect 3 binaries,
3 native tarballs, the model, and SHA256SUMS.

- `internal/buildinfo.Version` is stamped by the release build via ldflags —
  never edit it by hand.
- Bump `min_herdr_version` only when adopting new herdr APIs.
- Release assets can 504 for a minute or two right after publishing;
  `scripts/install.sh` retries through that window.
- Upgraded daemons self-replace on version mismatch (`hap daemon --ensure`
  detects a stale flock holder via the pid+version lock file); `hap status`
  flags a STALE daemon.

## Architecture rules (enforced)

- **`internal/domain` stays pure** — no imports of herdr/SQLite/LLM/adapter
  packages; `TestDomainPurity` fails otherwise. Side effects live behind the
  interfaces in `internal/ports` (implementations: `internal/herdr`,
  `internal/store`, `internal/llm`).
- **Optional capabilities are optional interfaces** — extend the herdr
  surface with a new port interface (see `LocatorPort`, `InspectorPort`) and
  type-assert at the call site, degrading gracefully; don't grow
  `HerdrPort` and break every fake.
- **Fail safe on the daemon path** — no panics; every error resolves to
  escalate + audit + log. Wrap new handler/adapter calls in `logging.Guard`.
- **Safety controls are never bypassed** — LLM submissions and learned rules
  alike are re-gated through kill switch, never-auto patterns, rate guard, and retry
  ceiling. Changes touching these must keep/extend the safety-invariant
  tests; new destructive-command shapes go in
  `internal/domain/testdata/irreversible_corpus.txt` (CI fails if seed
  patterns miss a corpus entry).
- **Don't stall the main loop** — the daemon's select loop handles all
  agents; anything that shells out repeatedly (LLM CLI, deep pane reads)
  belongs in a goroutine that funnels results back through a channel
  (see `consultLLM` / `llmResults`).
- **Semantic matching degrades, never blocks** — situations resolve to
  learned signatures via embedding + vector search over the MASKED salient
  content (`daemon.resolveSignature`, `internal/match`, `internal/embedder`),
  falling back to normalized-BM25 text matching, then to today's exact hash.
  `SignatureResult.Raw` is the never-remapped content hash (the LLM drift
  check depends on it); SQLite's `signature_embeddings` is the source of
  truth and the bleve index under `<state>/match-index` is a disposable
  cache (mem-only scorch does NOT serve KNN — keep it disk-backed). Embed
  calls are stall-guarded and latch a degraded mode after 3 consecutive
  failures.

## Testing practices

- Unit tests are mandatory for behavior changes — table-driven where natural,
  fakes over mocks (`internal/fakeherdr` fakes the herdr socket + CLI;
  `daemon_test.go` has in-process fakes and a `newHarness` helper).
- **Unix socket paths are length-capped** (~104 bytes on macOS): tests must
  use `testutil.SocketDir(t)`, never `t.TempDir()`, for socket paths.
- macOS temp dirs live under the `/var → /private/var` symlink — compare
  paths via `filepath.EvalSymlinks`, not string equality.
- Anything spawning real subprocesses should tolerate a deleted cwd
  (see `llm.Adapter.WorkDir` and `chdirStable`) — the daemon can outlive the
  directory herdr launched it from.

## herdr integration gotchas (verified against herdr 0.7)

- CLI reads print JSON envelopes (`{"id":…,"result":{…}}`); `pane read
  --format text` prints plain text. `pane get` exposes `cwd` /
  `foreground_cwd` (a deleted dir renders as `"/path (deleted)"`).
- `agent send` writes text WITHOUT Enter — follow with
  `pane send-keys <pane> enter`.
- **Numbered menus want the digit, not the label.** A Claude approval/choice
  (`1. Yes / 2. No`) only accepts the option's number; sending the literal
  label ("Yes") is silently ignored — it reads as "nothing happened" on
  confirm. Map the chosen option to its digit with `domain.MenuKeystroke`
  before delivering (both the daemon `act` and frontend confirm paths do).
- **`pane read --source recent` is a consuming delta**, not the screen: after
  one read (e.g. the daemon's classification read) it can return just the
  cursor line. To recover a standing menu at confirm time, read
  `--source visible` (`herdr.CLI.ReadPaneVisible` /
  `ports.VisiblePaneReader`).
- One `events.subscribe` per socket connection; status subscriptions require
  a concrete `pane_id`; existing panes are replayed as `pane_created`.
- Adding a pane makes the subscriber reconnect ("pane set changed", 1s
  backoff) — tests pushing transitions right after `AddPane` must wait past
  the resubscribe.
- The herdr binary is resolved via `HERDR_BIN_PATH` (fallback: `herdr` on
  PATH); the events socket via `HERDR_SOCKET_PATH`.

## Where things live

| Path | What |
|---|---|
| `cmd/hap` | entrypoint: daemon / TUI / CLI / `mcp` subcommands |
| `internal/domain` | pure decision core, signatures, safety heuristics |
| `internal/daemon` | monitor loop: subscribe → classify → decide → act/escalate |
| `internal/classify` | pane-content classifier + golden fixtures |
| `internal/llm` | operator LLM CLI adapter (argv template, auto-repair) |
| `internal/mcpserver` | stdio MCP server (`get_context`, `submit_decision`) |
| `internal/herdr` | herdr CLI + events-socket adapters |
| `internal/store` | SQLite persistence (WAL; `context_json` is an opaque blob) |
| `internal/fakeherdr`, `e2e_harness/` | test fakes and the e2e driver |
| `docs/specs/` | product/solution specs (FR-xxx / NFR-xxx ids used in comments) |
