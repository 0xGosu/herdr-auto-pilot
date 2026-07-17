# CLAUDE.md

Herd Auto Prompter (**hap**) — a Go plugin for the herdr terminal multiplexer
that watches every agent pane, auto-answers when a learned rule is confident,
and escalates to the operator (or a local LLM CLI) when not. `CONTRIBUTING.md`
has the full ground rules; this file is the day-to-day working reference.

## Skills (`.claude/skills/`)

Prefer these for how-to detail — this file keeps only what must stay in view.
- **`herdr`** — drive herdr from inside it (workspaces, tabs, panes, agents, waits).
- **`hap`** — operate the plugin via its CLI `hap`: agent's status, agent's tasks, escalations, config, safety rules, task
  sources.
- **`hap-development-local`** — the local dev loop: link the working tree, rebuild,
  hot-swap the daemon (`hap daemon --ensure`), and live-test against a real agent.

## Build, test, lint

The semantic matcher links native code (llama.cpp via CGO, FAISS behind bleve's
`vectors` tag), so **the native deps are needed once** and the `vectors cpu` tags
always — a build without both fails to link.

```sh
bash scripts/setup-native.sh                   # one-time: submodules + llama-go libs + FAISS → /usr/local/lib
go build -tags "vectors cpu" ./...             # CGO; needs a C/C++ toolchain
go test -tags "vectors cpu" ./... -count=1     # full unit/golden/safety/semantic suite (what CI runs)
gofmt -l . | grep -v submodule && go vet -tags "vectors cpu" ./...
golangci-lint run --build-tags "vectors,cpu"   # CI runs this too
```

- The real-model embedder test skips unless `models/all-minilm-l6-v2-q8_0.gguf`
  exists (download once from the HF repo in `release.yml`, or set `HAP_TEST_EMBED_MODEL`).
- Golden classifier fixtures: `internal/classify/testdata/`; regenerate with
  `UPDATE_GOLDEN=1 go test ./internal/classify/` and review the diff.
- Run the full suite before every commit that touches Go code.
- Full pipeline smoke test (fake herdr → real daemon → real LLM CLI):
  `go build -o /tmp/e2e ./e2e_harness && /tmp/e2e <short-dir> <hap-bin> <config-dir> <state-dir>`,
  then inspect with `hap audit` / replay `get_context` via `hap mcp`.
- Iterating on the plugin against a live herdr (link the working tree, rebuild,
  hot-swap the daemon): see the **`hap-development-local`** skill.

## Local integration suite (real herdr + claude)

`test/integration/` drives an **actual running herdr** (and, when enabled, a
**real Claude Code CLI**), gated by the `integration` build tag so `go test ./...`
and CI never run them. Each test **skips** (never fails) when its dependency is
absent, so these are safe to run anywhere:

```sh
go test -tags integration ./test/integration/ -v                    # from inside herdr, or set HERDR_BIN_PATH
HAP_ITEST_CLAUDE=1 go test -tags integration ./test/integration/ -v # also drive a real claude (spends tokens)
go test -tags "integration vectors cpu" ./test/integration/ -v      # include the real-model semantic case
```

- Loads `test/integration/testdata/config.toml` (the Claude Code recipe) — edit it
  to match the CLI you want to exercise.
- Cases: `TestRealPaneInfo` (herdr `pane get` → cwd/ids); `TestRealConfirmDeliversMenuDigit`
  (confirming a label reply selects the numbered menu — the send-content regression);
  `TestRealClaudeConsult` (needs `HAP_ITEST_CLAUDE=1`) drives a real claude
  (`--model haiku`, override `HAP_ITEST_CLAUDE_MODEL`) to an approval menu and asserts the
  menu digit reached it — skips if it can't elicit a prompt, so it needs a path OUTSIDE
  claude's auto-approved dirs (`/tmp`, `/workspaces`, `~/.claude`) and touches a `$HOME`
  dotfile; `TestRealEmbeddingSemanticMatch` (needs `vectors cpu`) drives a real llama.cpp
  model + FAISS index so a rule learned for one approval auto-answers a paraphrase
  (cosine ≥ 0.90) and leaves an unrelated one alone — skips without the model;
  `TestRealClaudePreviewMCQDelivery` (needs `HAP_ITEST_CLAUDE=1`) drives a real
  AskUserQuestion form whose options carry PREVIEWS and asserts the answers actually land —
  the rendering where a digit only moves the caret, which blind digit delivery no-oped on.

**Recommended: run the integration suite once after finishing any feature**, before the
PR — the unit suite fakes herdr, so only this catches real CLI-shape drift (e.g.
`pane read --source recent` vs `visible`, `agent send` delivering a digit vs a label).

## Commits

Format: `#<issue> <type>: <subject>` — a Conventional Commit prefixed with a GitHub
issue reference. A commit-msg hook **rejects messages that don't start with a
ticket/issue id**. Examples:

```
#1 feat: enrich LLM consult context — location ids, cwd, configurable pane excerpt
#1 fix: run daemon from state dir, self-heal stale daemons on upgrade
```

- Types: `feat`, `fix`, `docs`, `test`, `refactor`, `chore`; breaking → `feat!:`.
- Pre-commit hooks also check large files, secrets, trailing whitespace, and line
  endings — let them run (don't `--no-verify`).
- Never commit directly to `main`. Branch (`feat/…`, `fix/…`), open a PR.
- For any non-trivial change, use the **`git-worktree`** skill to create a new isolated
  worktree from `main` (`worktree-agent-noN` beside the repo) so `main`'s checkout stays
  clean; remove the worktree and delete the branch (local + origin) after merge.
- If the current repository has many uncommitted changes, or you detect or suspect that
  another agent is working in parallel in the same repository, pause before making more
  changes. Stage only your own changes, then use the **`git-worktree`** skill to create a
  new worktree from `main` that includes those staged changes, and continue there without
  disturbing the other work in progress.

## Version bump & release

Releases are **automated on merge to main** with a bump-then-tag model
(`.github/workflows/auto-release.yml`); `version` in `herdr-plugin.toml` is the single
source of truth and always names a version whose GitHub release exists — it TRAILS
releases, never leads them. This is load-bearing: `herdr plugin install` clones main and
`scripts/install.sh` downloads the release assets named by the manifest version, so a
manifest pointing at an unreleased version 404s every install.

- **Patch (the default)** — just merge your feature PR. The workflow finds the manifest
  version already tagged, auto-merges a bump PR (`release/bump-vX.Y.Z+1`, commit marked
  `[skip release]`), tags that bump commit with the owner's `RELEASE_PAT`, and the tag
  fires the standard tag-driven `release.yml`. Never bump the manifest for patch work.
- **Minor/major (the reserved manual path)** — overwrite `version` in `herdr-plugin.toml`
  INSIDE your feature PR (e.g. `0.4.0`); on merge the workflow finds that version untagged,
  skips the bump, and tags the merge commit directly. (The same branch self-heals a crashed
  run that bumped but never tagged.)
- Doc/workflow-only pushes (`**.md`, `docs/**`, `.github/**`) and merge commits containing
  `[skip release]` do not release. Hand-pushing a `v*.*.*` tag still works (release.yml is
  unchanged and tag-driven).
- Never put `[skip ci]`-family keywords ANYWHERE in the squash-merge message (title or
  body) of a PR that should release: GitHub suppresses ALL workflows for refs whose head
  commit carries one — including the tag push onto that commit, so the release silently
  never builds. The workflow refuses to tag such a commit. `[skip release]` (our custom
  marker) is safe on tagged commits but suppresses auto-release itself, so keep the literal
  string out of ordinary merge messages too.
- Between the bump merge and the release publishing (~15 min), installs from main can fail
  with 404 — install.sh's ~60 s curl retry only bridges the post-publish upload gap, not
  the build. Retry once the release publishes; pinned `--ref vX.Y.Z` installs are never
  affected.
- If the release BUILD fails after the tag exists, re-run the failed release.yml run; do
  not re-run auto-release (it would advance versions).

`release.yml` (tag-driven, unchanged) runs the full CI gate, then builds on THREE native
runners (CGO cannot cross-compile; Intel macOS is deliberately unsupported):
`hap-{linux-amd64,linux-arm64,darwin-arm64}` (llama.cpp statically linked in), a
`hap-native-<os>-<arch>.tar.gz` per platform (FAISS shared libs, plus libomp on macOS,
rpath'd to `<plugin>/lib`), the `all-minilm-l6-v2-q8_0.gguf` embedding model fetched from
Hugging Face (sha256-pinned), and `SHA256SUMS`; then publishes the GitHub Release.
`install.sh` treats the binary and native tarball as REQUIRED and the model as optional
(BM25 fallback).

The invariant: **the tagged commit's `herdr-plugin.toml` version and the git tag MUST
match** — the automation preserves it by construction (the tag always lands on a commit
whose manifest carries exactly that version).

- Verify after any release: `gh release view vX.Y.Z` — expect 3 binaries, 3 native
  tarballs, the model, and SHA256SUMS.
- `internal/buildinfo.Version` is stamped by the release build via ldflags — never edit
  it by hand.
- Bump `min_herdr_version` only when adopting new herdr APIs.
- Release assets can 504 for a minute or two right after publishing; `scripts/install.sh`
  retries through that window.

## Architecture rules (enforced)

- **`internal/domain` stays pure** — no imports of herdr/SQLite/LLM/adapter packages;
  `TestDomainPurity` fails otherwise. Side effects live behind the interfaces in
  `internal/ports` (implementations: `internal/herdr`, `internal/store`, `internal/llm`).
- **Optional capabilities are optional interfaces** — extend the herdr surface with a new
  port interface (see `LocatorPort`, `InspectorPort`) and type-assert at the call site,
  degrading gracefully; don't grow `HerdrPort` and break every fake.
- **Fail safe on the daemon path** — no panics; every error resolves to escalate + audit +
  log. Wrap new handler/adapter calls in `logging.Guard`.
- **Safety controls are never bypassed** — LLM submissions and learned rules alike are
  re-gated through kill switch, never-auto patterns, rate guard, and retry ceiling. Changes
  touching these must keep/extend the safety-invariant tests; new destructive-command shapes
  go in `internal/domain/testdata/irreversible_corpus.txt` (CI fails if seed patterns miss a
  corpus entry).
- **Don't stall the main loop** — the daemon's select loop handles all agents; anything that
  shells out repeatedly (LLM CLI, deep pane reads) belongs in a goroutine that funnels
  results back through a channel (see `consultLLM` / `llmResults`).
- **Attention events are delay-captured** — the classification pane read waits
  `[[capture_delay]]` (default 10s on an agent's first event, 2000ms after) via a per-pane
  `time.AfterFunc` → `delayedTr`, so the agent TUI has painted and event bursts coalesce
  (latest wins, one capture per burst). Daemon tests inherit a 1ms wildcard rule from the
  harness.
- **Semantic matching degrades, never blocks** — situations resolve to learned signatures via
  embedding + vector search over the MASKED salient content (`daemon.resolveSignature`,
  `internal/match`, `internal/embedder`), falling back to normalized-BM25 text matching, then
  exact hash. `SignatureResult.Raw` is the never-remapped content hash (the LLM drift check
  depends on it); SQLite's `signature_embeddings` is the source of truth and the bleve index
  under `<state>/match-index` is a disposable cache (mem-only scorch does NOT serve KNN — keep
  it disk-backed). Embed calls are stall-guarded and latch a degraded mode after 3 consecutive
  failures.

## Testing practices

- Unit tests are mandatory for behavior changes — table-driven where natural, fakes over
  mocks (`internal/fakeherdr` fakes the herdr socket + CLI; `daemon_test.go` has in-process
  fakes and a `newHarness` helper).
- **Unix socket paths are length-capped** (~104 bytes on macOS): tests must use
  `testutil.SocketDir(t)`, never `t.TempDir()`, for socket paths.
- macOS temp dirs live under the `/var → /private/var` symlink — compare paths via
  `filepath.EvalSymlinks`, not string equality.
- Anything spawning real subprocesses should tolerate a deleted cwd (see `llm.Adapter.WorkDir`
  and `chdirStable`) — the daemon can outlive the directory herdr launched it from.

## herdr integration gotchas (verified against herdr 0.7)

The **`herdr`** skill covers CLI usage; these are the hap-specific protocol facts.

- CLI reads print JSON envelopes (`{"id":…,"result":{…}}`); `pane read --format text` prints
  plain text. `pane get` exposes `cwd` / `foreground_cwd` (a deleted dir renders as
  `"/path (deleted)"`).
- `agent send` writes text WITHOUT Enter — follow with `pane send-keys <pane> enter`.
- **Numbered menus want the digit, not the label.** A Claude approval/choice (`1. Yes / 2. No`)
  only accepts the option's number; sending the literal label ("Yes") is silently ignored — it
  reads as "nothing happened" on confirm. Map the chosen option to its digit with
  `domain.MenuKeystroke` before delivering (both the daemon `act` and frontend confirm paths do).
- **A digit does NOT always commit — AskUserQuestion has two protocols, per tab.** Verified live
  (2026-07-16): on **plain** options (`1. Apple / 2. Banana`) the digit selects AND auto-advances,
  but on **preview** options (option list left, `┌──┐` preview box right, `Notes: press n to add
  notes`) the digit only **moves the caret** like ↑/↓ — **Enter** commits and advances. The footer
  is identical in both and never mentions digits, and one form mixes them (a preview form's
  generated Submit tab renders plain). Blind digit-only delivery is a silent no-op on preview
  forms: nothing is answered and the agent stays blocked. Never plan a whole keystroke series up
  front — `internal/mcqdeliver` presses the digit, re-reads, and only presses Enter if the answer
  did not commit (and refuses if the caret never reached the chosen option).
- **`pane read --source recent` is a consuming delta**, not the screen: after one read (e.g. the
  daemon's classification read) it can return just the cursor line. To recover a standing menu at
  confirm time, read `--source visible` (`herdr.CLI.ReadPaneVisible` / `ports.VisiblePaneReader`).
- One `events.subscribe` per socket connection; status subscriptions require a concrete
  `pane_id`; existing panes are replayed as `pane_created`.
- Adding a pane makes the subscriber reconnect ("pane set changed", 1s backoff) — tests pushing
  transitions right after `AddPane` must wait past the resubscribe.
- The herdr binary is resolved via `HERDR_BIN_PATH` (fallback: `herdr` on PATH); the events
  socket via `HERDR_SOCKET_PATH`.

## Where things live

| Path | What |
|---|---|
| `cmd/hap` | entrypoint: daemon / TUI / CLI / `mcp` subcommands |
| `internal/domain` | pure decision core, signatures, safety heuristics |
| `internal/daemon` | monitor loop: subscribe → classify → decide → act/escalate |
| `internal/classify` | pane-content classifier + golden fixtures |
| `internal/mcqdeliver` | answers a live multi-tab MCQ form, verifying each keystroke landed |
| `internal/llm` | operator LLM CLI adapter (argv template, auto-repair) |
| `internal/mcpserver` | stdio MCP server (`get_context`, `submit_decision`) |
| `internal/herdr` | herdr CLI + events-socket adapters |
| `internal/store` | SQLite persistence (WAL; `context_json` is an opaque blob) |
| `internal/fakeherdr`, `e2e_harness/` | test fakes and the e2e driver |
| `docs/specs/` | product/solution specs (FR-xxx / NFR-xxx ids used in comments) |
