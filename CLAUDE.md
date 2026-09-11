# CLAUDE.md

Herd Auto Prompter (**hap**) — a Go plugin for the herdr terminal multiplexer that
watches every agent pane, auto-answers when a learned rule is confident, and escalates
to the operator (or a local LLM CLI) when not. `CONTRIBUTING.md` has the full ground
rules; this file is the day-to-day working reference.

**How to read the architecture rules.** Each names the identifier that implements it and
states which way it must fail. The full rationale — mechanism, measured numbers, the
incident that produced it — is the doc comment on that identifier, which is usually
richer than what is here; go read it before changing anything the rule covers. What this
file adds is reach: the hazard is normally in a file you had no reason to open, and the
**test traps** are the reason each of these regressions shipped green. Find the guarding
tests with `grep -rn "func Test<Topic>" --include=*_test.go`.

## Skills (`.claude/skills/`)

Prefer these for how-to detail.
- **`herdr`** — drive herdr from inside it (workspaces, tabs, panes, agents, waits).
- **`hap`** — operate the plugin via its CLI: status, tasks, escalations, config, safety
  rules, task sources.
- **`hap-development-local`** — the local dev loop: link the working tree, rebuild,
  hot-swap the daemon (`hap daemon --ensure`), live-test against a real agent.

The hap skill also ships in the binary: `hap --skill` prints it,
`hap skill install <claude|codex|agy|agents>...` (or the TUI Config tab) installs it.

## Build, test, lint

The semantic matcher links native code (llama.cpp via CGO, FAISS behind bleve's `vectors`
tag), so **the native deps are needed once** and the `vectors cpu` tags always — a build
without both fails to link.

```sh
bash scripts/check-submodule-gitlink.sh        # submodule must be a gitlink, not a symlink (#265)
bash scripts/setup-native.sh                   # one-time: submodules + llama-go libs + FAISS → /usr/local/lib
go build -tags "vectors cpu" ./...             # CGO; needs a C/C++ toolchain
go test -tags "vectors cpu" ./... -count=1     # what CI runs
gofmt -l . | grep -v submodule && go vet -tags "vectors cpu" ./...
golangci-lint run --build-tags "vectors,cpu"
```

- The real-model embedder test skips unless `models/all-minilm-l6-v2-q8_0.gguf` exists
  (download once from the HF repo in `release.yml`, or set `HAP_TEST_EMBED_MODEL`).
- Golden classifier fixtures: `internal/classify/testdata/`; regenerate with
  `UPDATE_GOLDEN=1 go test ./internal/classify/` and review the diff.
- Run the full suite before every commit that touches Go code.
- Profiling (opt-in, any verb): `HAP_PROFILE_DIR=<dir> [HAP_PROFILE_SECONDS=60] hap daemon --restart` writes
  ROLLING `<verb>-<pid>.cpu.pprof` / `.heap.pprof` windows (`internal/profiling`) — the files are always the latest
  complete window, so an idle daemon hours in can be read with `go tool pprof -top bin/hap <file>`. The detached
  daemon inherits the environment; nothing is written or listened on when the variable is unset. Each window also
  writes `<verb>-<pid>.mem.txt` (Go memory classes, plus `/proc/self/status` on Linux): the heap profile sees only LIVE Go
  objects, a few MB of a resident set mostly made of file-backed pages, freed-but-unreturned Go heap and native
  memory (Turso engine, FAISS, llama.cpp) — "go resident" is what to subtract from RssAnon to find the native part.
- Pipeline smoke test (fake herdr → real daemon → real LLM CLI):
  `go build -o /tmp/e2e ./e2e_harness && /tmp/e2e <short-dir> <hap-bin> <config-dir> <state-dir>`.

## Local integration suite (real herdr + claude)

`test/integration/` drives an **actual running herdr** (and, with `HAP_ITEST_CLAUDE=1` /
`HAP_ITEST_CODEX=1` / `HAP_ITEST_AGY=1`, a real agent CLI), gated by the `integration` build tag so
`go test ./...` and CI never run them. Each case **skips** (never fails) when its
dependency is absent.

```sh
go test -tags integration ./test/integration/ -v                    # from inside herdr, or set HERDR_BIN_PATH
HAP_ITEST_CLAUDE=1 go test -tags integration ./test/integration/ -v -timeout 20m  # spends tokens
HAP_ITEST_AGY=1 go test -tags integration ./test/integration/ -run TestRealAgy -v  # Gemini tokens
go test -tags "integration vectors cpu" ./test/integration/ -v      # + the real-model semantic case
```

**Recommended: run this once after finishing any feature**, before the PR — the unit suite
fakes herdr, so only this catches real CLI-shape drift.

Three traps, each of which already cost a shipped regression:

- **Anything asserting a CONFIRM must run its own daemon; an `App` with no `DaemonInfo` is
  the trap.** `AssessDaemonHealth` derives `Running` from `DaemonInfo`, so nil reads as "no
  daemon" whatever is actually up and `requireLiveDaemonFor` refuses while naming a daemon
  that IS running. `testDaemon.App` is the one correct wiring — and it deliberately holds NO
  herdr adapter, so keystrokes landing at all is the proof delivery went through the daemon.
  Never point such a test at the operator's live store: under turso that pushes scratch rows
  to their cloud database.
- **A scratch pane the DAEMON will classify must SCROLL.** `pane read --source recent`
  returns EMPTY for a pane whose output still fits on screen (verified live, herdr 0.8.2);
  a live claude pane returns kilobytes, which is why production never meets this. Use
  `fillViewportSh` — its filler is digit-free because a salient masked mostly to
  placeholders trips the over-masking floor, the same failure by the other door.
- **Wait for a pane's CONTENT (`waitForPaneText`), never sleep.** `pane run` only hands the
  command to the shell, and the startup reconcile classifies an unpainted pane as empty,
  whose escalation then stops the injected transition re-capturing. A 1s sleep held in
  isolation and lost under full-suite load.

- **On a working machine the OPERATOR's daemon watches the scratch panes too.** It answers agy forms
  (under full self-prompting within seconds), so the agy cases `hap disable` their pane
  (`quietOperatorDaemon`) — and it reads `--source recent`, the same consuming delta, which is why
  the agy hand-out case goes through the operator path (visible reads only) rather than the idle poll.
  A front end also needs a PUBLISHED roster to resolve any target (#386): `modeApp` publishes herdr's
  listing (`publishLiveRoster`), which is what `TestRealClaudeModeCycle` lacked while it failed.

Cases worth knowing beyond their names: `TestRealShiftTabKeyNameIsStillBroken` is a
**tripwire** — it FAILS if herdr's `shift+tab` key name starts working, the signal to delete
`domain.ShiftTab`/`CLI.SendChord`. `TestRealClaudeOneQuestionMultiSelectMCQDelivery` FAILS
(does not skip) when a form is on screen but `MultiTabForm` misses it, so that detection
regression can never pass silently. `TestRealClaudeConsult` needs a path OUTSIDE claude's
auto-approved dirs (`/tmp`, `/workspaces`, `~/.claude`) to elicit a prompt. The mode-cycle
cases DISCOVER a session's cycle rather than assuming `AgentModesFor`, and are the only check
that the Shift+Tab chord ENCODING still reaches an agent and that the mode INDICATOR still
renders the labels the parser matches — both live in the agent's build, not herdr's.

## Commits

Format: `#<issue> <type>: <subject>` — a Conventional Commit prefixed with a GitHub issue
reference; a commit-msg hook rejects anything else. Types: `feat`, `fix`, `docs`, `test`,
`refactor`, `chore`; breaking → `feat!:`.

- Let the pre-commit hooks run (large files, secrets, whitespace, line endings) — no
  `--no-verify`.
- Never commit directly to `main`. Branch (`feat/…`, `fix/…`), open a PR.
- For any non-trivial change use the **`git-worktree`** skill (`worktree-agent-noN` beside the
  repo); remove it and delete the branch (local + origin) after merge.
- If the repo has many uncommitted changes, or another agent may be working in parallel:
  pause, stage only your own changes, and move to a new worktree from `main` that includes them.

## Changelog (MANDATORY)

**Every change gets an entry, patch releases included.** Entries are **fragments in
`changelog.d/`**, never edits to `CHANGELOG.md`, and **never a version number**:

```sh
cat > changelog.d/$(git branch --show-current | tr / -).md <<'EOF'
- Fixed the thing that used to happen
EOF
```

- **Why fragments:** every PR used to insert its section at the top of one shared file, so two
  open PRs always conflicted even when unrelated — and each had to GUESS its version, which is
  only knowable when the release is cut.
- On merge, auto-release runs `scripts/assemble-changelog.sh <version>` inside the same commit
  that bumps `herdr-plugin.toml`. CI fails any PR that changes releasing code without a fragment.
- **Minor/major is the manual exception**: no bump commit is created to assemble into, so run
  `bash scripts/assemble-changelog.sh X.Y.0` in that same PR and commit the result. The release
  refuses to tag while unassembled fragments remain.
- Style: flat verb-first one-liners (`Added …`, `Fixed …`), no sub-sections. Write what it MEANS
  for the reader — GitHub already generates a list of PR titles. Mark breaking changes
  **Breaking.**

## Version bump & release

Releases are **automated on merge to main** with a bump-then-tag model
(`.github/workflows/auto-release.yml`); `version` in `herdr-plugin.toml` is the single source of
truth and always names a version whose release exists — it TRAILS releases, never leads them.
Load-bearing: `install.sh` downloads the assets named by the manifest version, so a manifest
pointing at an unreleased version 404s every install.

- **Patch (the default)** — just merge; the workflow auto-merges a bump PR (marked
  `[skip release]`) and tags it. Never bump the manifest for patch work.
- **Minor/major** — overwrite `version` in `herdr-plugin.toml` INSIDE your feature PR; the
  workflow finds it untagged and tags the merge commit. (The same branch self-heals a crashed run
  that bumped but never tagged.)
- Doc/workflow-only pushes (`**.md`, `docs/**`, `.github/**`) and merge commits containing
  `[skip release]` do not release.
- **Never put `[skip ci]`-family keywords ANYWHERE in a squash-merge message** of a PR that should
  release: GitHub suppresses ALL workflows for that ref, including the tag push, so the release
  silently never builds. `[skip release]` is our own marker — safe on tagged commits, but keep it
  out of ordinary merge messages.
- Between the bump merge and publishing (~15 min) main's manifest names a version with no assets,
  so install.sh **falls back to the newest earlier release that has assets** and warns loudly.
  Refused for an explicit `HAP_VERSION` pin, under `HAP_NO_FALLBACK` (any NON-EMPTY value —
  set-but-empty means unset), and on a checksum mismatch (corruption is never answered with a
  downgrade). `--ref vX.Y.Z` is NOT a refusal: it pins the git clone, which install.sh cannot see.
  Detached HEAD is not a usable signal either, because auto-release tags the bump commit on main.
- If the release BUILD fails after the tag exists, re-run the failed release.yml run; do not re-run
  auto-release (it would advance versions).

`release.yml` (tag-driven) builds on THREE native runners (CGO cannot cross-compile; Intel macOS is
deliberately unsupported): three binaries, a native tarball per platform (FAISS shared libs, plus
libomp on macOS, rpath'd to `<plugin>/lib`), the sha256-pinned embedding model, and `SHA256SUMS`.
install.sh treats the binary and native tarball as REQUIRED and the model as optional (BM25
fallback).

**The invariant: the tagged commit's manifest version and the git tag MUST match.** Verify with
`gh release view vX.Y.Z` — 3 binaries, 3 tarballs, the model, SHA256SUMS. `internal/buildinfo.Version`
is stamped by ldflags — never edit by hand. Bump `min_herdr_version` only when adopting new herdr
APIs. Assets can 504 for a minute after publishing; install.sh retries through that window.

## Architecture rules (enforced)

### Boundaries

- **`internal/domain` stays pure** — no imports of herdr/SQLite/LLM/adapter packages
  (`TestDomainPurity`). Side effects live behind `internal/ports`.
- **Optional capabilities are optional interfaces** — add a port interface (see `LocatorPort`,
  `InspectorPort`) and type-assert at the call site, degrading gracefully; don't grow `HerdrPort`
  and break every fake.
  - **Test trap:** the daemon suite's `failingStore` embeds the `ports.StorePort` INTERFACE, so a
    type-asserted capability not forwarded there is silently off suite-wide. `RowRetentionPort` is
    separate from `RetentionPort` for the same reason.
- **Fail safe on the daemon path** — no panics; every error resolves to escalate + audit + log.
  Wrap new handler/adapter calls in `logging.Guard`.
- **Safety controls are never bypassed** — LLM submissions and learned rules alike are re-gated
  through kill switch, never-auto patterns, rate guard and retry ceiling. New destructive-command
  shapes go in `internal/domain/testdata/irreversible_corpus.txt` (CI fails if seed patterns miss one).
- **Don't stall the main loop** — the select loop serves all agents; anything shelling out repeatedly
  (LLM CLI, deep pane reads) belongs in a goroutine funnelling results back through a channel
  (`consultLLM` / `llmResults`).
- **Egress has exactly three exceptions, all opt-in and off by default** — `internal/updatecheck`,
  the `github_gist` task backend (task text only), and the `turso` engine (syncs the WHOLE store to
  the operator's Turso Cloud database). `internal/privacy` bans the GitHub and Turso SDKs by import
  path as well as `net/http`, because the walker checks DIRECT imports — an adapter using only an SDK
  would egress while passing, and the Turso SDK's network code is native so nothing else could catch
  it. The gist adapter must keep using `github.WithURLs` and `github.WithTimeout` (not a `*url.URL`
  or a hand-built transport), or `net/url` and the no-remote-dial scan need widening.

### Config surface

- **Everything that writes config.toml is a `hap config` subcommand**, which is why it is the ONLY
  visible command in the help's Configure group. Topics are TWO-word registry entries resolved
  longest-spelling-first in `cli.Run` — that is what makes `hap config rules --help` reach the
  topic's own page. `hap task` is deliberately NOT here: it edits checklist ITEMS, not config. Old
  top-level spellings still resolve (`Command.MovedFrom`) and print their migration note on
  **stderr** — these verbs print tab-separated listings that scripts parse.
- **Every config key is reachable from the CLI** — the TUI is a convenience, not a capability.
  SCALARs go in `frontend.ConfigFields`; ARRAY/MAP sections cannot be a `config set` key (an element
  is addressed by POSITION, a map entry by NAME) so each gets a verb named in `configListCommands`.
  Both registry tests fail on a STALE entry too. Four rules bind the list editors:
  - **A long-lived list ELEMENT's own fields are held to the same standard** — every
    `[[task_sources]]` field has a `set` key, because settable-only-at-creation means
    remove-and-re-add: retyping every other field and renumbering every later source to change one.
  - **Removal compares the WHOLE entry the caller listed**, never one field — a one-field guard
    passes on the wrong element exactly when a listing has gone stale, and duplicate-ish entries are
    legitimate.
  - **An insert must respect the daemon's lookup order** — `config.CaptureDelay` takes the first rule
    matching the agent type and `"*"` matches everything, so a specific rule appended after a wildcard
    one is configured, listed, and never read.
  - **`[[task_sources]]` inverts that: a new source is always APPENDED.** Its index is a public
    selector (`hap task 2 …`, `{task_source_index}` in delivered prompts, every listing/set/remove),
    so inserting elsewhere re-points every selector after it — including commands already in an
    agent's scrollback. Binds BOTH creating surfaces: `AddTaskSource` and `addTaskSourceIfAbsent`
    (accepting an LLM task suggestion registers a source as a side effect).
  - Secrets are a DISPLAY rule, not an exemption: `hap config env` never prints a value and reads it
    from stdin unless `--value` is passed.

### A front end decides; the DAEMON does

Anything that reaches a live pane, or is keyed by an identifier only one machine can resolve, is
written to `agent_actions` for the OWNING node and executed there (`domain.AgentAction`,
`daemon.executeAgentAction`). A generated-task confirm is the widest case and the reason this is not
only about panes: it matches a pane id against a herd (herdr recycles pane ids, so every machine has a
pane `1`), mints an `agent_names` row in a node-keyed table, writes a checklist, and registers a
`[[task_sources]]` entry in a config.toml that **never enters the shared database**.

- **Queued UNCONDITIONALLY, local rows included** (`queueGeneratedTaskConfirm`). The `isSelf` fast path
  the per-agent verbs use is wrong here because this path ends in a pane send: in-process it leaves a
  TUI or CLI holding a herdr adapter, and a front end and a daemon typing into one pane is the race
  none of the delivery guards can see. Accepted cost: `hap confirm <id>` without `--send` needs a live
  daemon.
- **The pane access is RECEIVED, never held** (`ports.TaskSendHost`) — the checklist, config and
  reserve→send→roll-back ORDERING stay in `internal/frontend`; only `cmd/hap`'s daemon wiring can build
  the closure that reaches a pane.
- **`ConfirmGeneratedTask` and `AcceptGeneratedTask` are two seams on purpose**: the automatic path
  passes `automated=true`, skipping BOTH `ResolveEscalation` and `InsertCorrection` — a machine's
  decision to act is not evidence the suggestion was right. `author` is threaded from the queued row,
  not the daemon's own App.
- **`CorrectionID` stays 0 on the queued row**; populating it arms `finishWithdrawn`, whose refusal
  DELETES the correction the first attempt wrote.
- **`side_effect` is marked inside the host's `Send`, not before the seam** — a row carrying it is
  FAILED at next start rather than replayed, which is true after keystrokes and false before them. The
  FSP path is handed `taskSendHost(0)` and the zero guard is load-bearing: marking action 0 writes
  against a row that does not exist.
- **The staleness gate moved WITH the pane access** — `refuseIfAgentBusy` lives in the daemon, and its
  refusal carries `domain.SuggestionStaleMarker` because `AwaitAgentAction` flattens every sentinel
  through `errors.New`. That one is ACTIONABLE: the TUI offers to add the tasks to the list instead,
  keyed on `errors.Is`, and dropping the marker breaks the offer SILENTLY.
- **The manual hand-out moved the same way** (`send_task`): `SendTaskToAgentOn` carries the agent's
  NAME, never a pane id, and the executor re-derives everything from the owning node's config and a
  live listing. Its idle re-check fails CLOSED and TERMINALLY — "we could not ask" is not "it is idle",
  and an operator blocking on the row must not wait three sweeps. `taskSourceRenderFor` matches by
  LOCATOR, not agent alone, or one source's item renders through another's template. `hap task send`
  is local-only; the TUI's Tasks tab can hand out a remote node's item.
- The operator path is deliberately NOT screened (`screen` is nil): the daemon's own sends are screened
  because no human saw the text; here one has, and their confirm has always been the gate. **The one
  exception is the orchestrator** (`domain.OrchestratorAuthor`, set by `cmd/hap` when HERDR_PANE_ID is the
  orchestrator's pane): an LLM's confirm is not a human's, so its generated-task confirms and `task send`
  get the daemon's `screenOutbound` (`daemon.actionScreen`) and all three executors refuse it while the herd
  is paused (`refuseOrchestratorWhilePaused`). Keyed on the queued row's AUTHOR, so an operator is untouched.
- **Test trap:** `internal/daemon` may not import `internal/frontend`, so its tests drive a FAKE seam
  and prove only the EXECUTOR's guards. The confirm's own behaviour is proved in `internal/frontend`;
  the TUI and CLI suites run a stand-in drain calling the REAL confirm, or their "the tasks file was
  written" assertions check nothing.

### Multi-tab forms and auto-accept

- **A multi-tab form's baseline is the swept AGGREGATE, so nothing may compare it to one frame.**
  `sweepFrames` walks every tab and `AggregateMCQFrames` joins them into the `Situation.Content` that
  mints the signature and is stored as `pane_excerpt`; anything re-reading the pane later holds a
  SINGLE frame, and a `choice` salient is STRUCTURED so the mismatch reads as proof the situation moved
  on. Guard 3 did exactly this and auto-dismissed EVERY multi-tab escalation as `auto_dismiss_stale` —
  deterministic, never intermittent, invisible because it looked like the feature working. Compare
  FRAME-WISE instead: same tab count, live `ExtractAgentMCQForm` equal to one of
  `AggregatedMCQFrames(excerpt)`. Guard 3 widens that two ways because it runs after a WAIT, and **each
  widening buys a gate — all four load-bearing:**
  - `NormalizeMCQFrame` folds the caret AND the preview box (on a preview tab the box is a FUNCTION of
    the focused option, so folding the caret alone fixes nothing).
  - The suggestion must be a series of exactly `AnswerCount` digits, checked against each captured
    tab's `domain.MultiSelectTab` mode — otherwise `deliver.Deliver` falls to the plain-menu path and
    maps the reply against whichever tab is visible, and a comma group (`1,3`) on a later tab is caught
    only after earlier tabs are already committed: a half-answered form. Such rows really exist and were
    simply unreachable while the guard always said no: `unfamiliar_options` leaves a wrong-shaped LLM
    answer pending WITH the answer attached, and is not in `autoAcceptExcludedReasons`.
  - `domain.MCQFormFullyUnanswered` — asked BEFORE the comparison, since `NormalizeMCQFrame` folds the
    marks away and delivery retypes EVERY tab from tab 1.
  - `domain.LooksLikeAggregatedMCQ` plus the `excerptTruncationMarker` prefix — treating "not a
    complete aggregate" as "some other capture" drops straight back into the original bug.

  All four refuse as `heldStillUnevaluable` (PENDING), never `heldStillNo`: a malformed answer, a
  half-answered form and a mangled capture each need a human. **Note:** every other multi-tab fixture
  here renders options without previews — `internal/domain/testdata/mcq_preview_*.txt` is the only pair
  reproducing the layout, which is why this shipped green.
- **A swept AGGREGATE is the one capture whose HEAD is load-bearing, so it gets its own storage budget**
  (`aggregateMaxRunes`, `excerptBudget`). Every other excerpt is a pane TAIL, which is why
  `truncateTailRunes` keeps the tail; an aggregate is the opposite shape, so shearing the head does not
  degrade the capture but DESTROYS it — it becomes unparseable, and `mcqFormHeldStill`
  then answers `heldStillUnevaluable` on every sweep forever at Debug level only — neither delivered nor
  dismissed. Three bounds: the budget gate is a **strict parse**, never `LooksLikeAggregatedMCQ` (which
  stays the READER's evidence of a mangled row); `SaveSignatureSnapshot` deliberately keeps the smaller
  cap because `signature_snapshots` has NO retention path; and `duplicatePendingEscalation` still passes
  `snapshotMaxRunes` as its `snapshotCap` ARGUMENT, where it is a fuzzy-dedup threshold rather than a
  storage budget.
  - **Test trap:** the seeder `seedAgedSweptEscalation` was the only writer of that column that skipped
    truncation, which is why every existing test was blind.
- **An option label stops at the preview column** (`trimPreviewColumn`) — a pane is one flat grid, so a
  preview box lands inside the option's own line and line-anchored parsing gave labels that changed on
  every caret move, minting a new signature each time. The cut only removes a SUFFIX, so a `[ ]`/`[✔]`
  prefix always survives; a bare `─` is excluded from the glyph set because agents draw separator rules
  with it.
  - **A label that trims to nothing is recovered from the next row, never dropped**
    (`wrappedOptionLabel`) — dropping it loses a choice the agent is really offering, so the option set
    reads one short and "2" maps to nothing. Recovery takes the FIRST continuation row only and requires
    that row to carry a preview column, because an ordinary menu's indented line is a DESCRIPTION (the
    repo's own 3-tab fixture renders that way).
  - Wrapped rows are deliberately NOT stitched into one label: a terminal wrap is lossy in a way that
    depends on where the text ends relative to the gutter, so no separator rule is correct at every width.
- **A checkbox tab is answered by TOGGLING, so its baseline is a safety control.** A blind digit is not
  idempotent — over a pane already carrying an attempt's toggles it CLEARS them and the advance submits
  an empty answer. The rule is `checked ⊆ chosen`, enforced at DELIVERY (`domain.CheckedOutside` in
  `daemon.reverifyMultiSelect`, `frontend.verifyTabBaseline`, per keystroke in `mcqdeliver.toggleTab`).
  CAPTURE only records — refusing there strands every form hap itself half-answered. The signature folds
  checkbox state away (`domain.NormalizedOptionSet`), so a half-delivered form still matches the rule
  learned for the untouched one. **The widened
  baseline needs evidence**: it applies only where this daemon recorded its own attempt at this
  pane+signature (`markToggleAttempt`, in-memory, lost across restarts — fails safe); without it a tab
  must be completely clean, or the rule also accepts an operator halfway through ticking that form.
- **The last look before a claim is COMPLETE** (`claimBlockedBy`) — everything above
  `ClaimForAutoAccept` is a herdr shell-out with a budget in SECONDS, so it re-asks all three controls
  broadest-first: kill switch (FR-017, not FSP-specific, fails closed on a read error), mode + ceiling
  latch (`stillPermitted`), and `accept_generated_task` for a generated-task row (a SEPARATE opt-in
  resolved once per sweep). `retireNoopEscalation` takes the same look — Guard 1a returns before ANY
  dismissal precisely so pausing the herd never destroys the queue it protects.
  - **A content-safety refusal is not a delivery fault** — tagged `errOutboundRefused` and handled like
    `errAgentDisabled`. It used to enter the retry budget, so the same never-auto match was refused
    `maxAutoAcceptAttempts` times and the row dismissed as `auto_accept_failed`, deleting exactly the
    escalation FR-015 says must always reach a human. Such a refusal is permanent by nature, so a budget
    could only ever end there.
  - **The compensating revert needs an obligation of its own.** `auto_accepting` is TRANSIENT and
    filtered out of both the operator's queue and the candidate query, and the only automatic reclaim
    runs at daemon START — so a failed `revertClaim` hides the escalation from everyone until a restart.
    Failed reverts are retried every tick (`retryAutoAcceptRevert`), NOT gated on the kill switch: it is
    bookkeeping about a claim already abandoned. Only a non-nil error is a failure; `false, nil` means
    another writer moved the row.
- **Every auto-accept refusal names itself once** — `notePending` logs at INFO per (row, reason). Before
  it, Guard 1b and the pane-busy skip produced no output at ANY level, which is why the
  truncated-aggregate bug took a five-round investigation. Per (row, reason), not per sweep — a changed
  reason is new information. All call sites sit AFTER `stillEligible` is set: an INELIGIBLE row
  `continue`s before that, so logging one there would be pruned and re-logged every minute forever.

### Full self-prompting (FSP)

**FSP may widen the YES side of Guard 3, never the NO side.** Two extra evidence paths, both reached
ONLY after the ordinary comparison refused, both gated on the `fsp bool`, and both required to resolve
to `heldStillUnevaluable` (PENDING) when they decline — a fallback answering `heldStillNo` turns a
widening into a queue-destroying dismissal.

- `mcqSalientHeldStill` answers a row truncated past its head using the byte-intact blocks
  (`domain.SurvivingMCQFrames`), keeping the SAME frame-wise relation the intact path uses. **The option
  set alone is NOT sufficient identity and must never become the only gate**: it is the union over every
  tab and every AskUserQuestion form ends in a generated `Submit answers`/`Cancel` tab, so a pane parked
  on ANY form's Submit tab is a subset of ANY other form's set. `domain.LiveMCQMatchesSalient` is only a
  cheap extra conjunct catching option drift, and must derive BOTH sides through `MaskVolatile` — the
  stored salient was masked, so an unmasked live side silently no-ops for every label carrying a path or
  number. Three further gates are not optional: tab count = series length; form fully unanswered; and
  **no token may be a comma group**, since the missing blocks carry no select mode to verify against. A
  capture with NO surviving block stays pending — a deliberate limit.
- `unstructuredHeldStill` answers a PANE-TAIL row via `domain.TailSimilarWithin`, which aligns both
  salients from the TAIL before comparing. The mismatch is structural, not statistical: the baseline
  comes from a `--source recent` CONSUMING DELTA while every re-read is `--source visible`, so symmetric
  Jaccard is dominated by content only the longer side ever had — the documented reason idle and
  generated-task rows never auto-accept. Aligning on the TAIL rather than testing containment is what
  keeps it safe: a screen that moved on paints its new content at the BOTTOM, inside the compared window.
  `domain.MinTailCompareRunes` is the floor, load-bearing for the same reason `min_salient_chars` is.
  `fspTailHeldStillJitterPercent` is a **separate constant** from `staleDeferredSendJitterPercent` at the
  same value — the loosening is the alignment, not the tolerance. Running after `SignatureHeldStill`
  refused, it must re-ask the two refusals its caller did not: either side over-masked (repeated
  placeholders share almost every trigram, so they clear any tolerance at any window — the magnet
  failure by a door the length floor does not cover), or a fresh salient that has become structured.

Outside Guard 3, a `@noop` suggestion under FSP is RETIRED (`ReasonAutoDismissNoop`) rather than left
pending: the sentinel means SEND NOTHING and can never become deliverable, so under a mode whose premise
is that nobody reads the queue it would sit forever. It is the only auto-accept path acting with no pane
evidence, and can afford to — nothing is typed, nothing is learned. It still honours the kill switch,
per-agent disable and runaway pause.

**The two opt-in keys are OFF by default, and each buys one narrow thing** —
`full_self_prompting.honour_limits` and `…accept_generated_task`.

- **`honour_limits = false` means the whole `[limits]` section is INERT, and inertness is one CLAUSE in
  each gate, never an early return.** The key used to skip only the mode's pre-check, so the ordinary
  path kept running the runaway guard: an agent at `max_consecutive_auto_prompts` was escalated
  `rate_limited` AND paused until a human checked in — and `rate_limited` is in
  `autoAcceptExcludedReasons`, making that escalation permanently operator-only. `domain.RateLimits.Inert`
  short-circuits `CheckRate` ahead of the `Paused` branch and gates the FR-014 retry ceiling in `Decide`
  (`limits.max_error_retries` is in the same section, so without that `retry_exhausted` is never raised).
  Deliveries still ADVANCE the counters. Resolve it through `limitsInert`, or `limitsInertFor` when an
  `fspActive` answer is in hand — a per-AGENT loop must use the latter, since `fspActive` owns the
  once-per-episode degradation warning.
  **The three gates reading `rate.Paused` are SHARED** — `sweepAllowed`, `autoAcceptAgentSuppressed`,
  `eligibleIdleAgents` each carry the per-agent disable (and `sweepAllowed` the kill switch and never-auto
  screen) in the same run of checks — so inertness relaxes the pause clause ALONE. An early return at the
  top of any of them is a safety bypass that still passes an end-to-end test, because the disable is
  re-checked at delivery; drive these guards DIRECTLY in tests.
- **A busy pane is not a runaway ceiling** — `domain.ReasonPaneBusy`, not `ReasonRateLimited`, at all four
  `acquirePane` failure sites. Borrowing the ceiling's tag pauses the agent over a lock the in-flight
  interaction releases on its own, and `autoAcceptExcludedReasons` then refuses the row forever.
  `pane_busy` is deliberately NOT excluded — `d.paneBusy` and Guard 3's re-read gate the retry.
- **The ceiling check must not read a PAUSE as a ceiling** — `CheckRate`'s first branch answers
  `rate_limited` for a paused agent, but `fspCeilingReached` runs BEFORE Guard 1b, which already suppresses
  those. Copy the rate with `Paused` cleared; only the two counters decide, or one paused agent switches
  the mode off for the whole herd.
- **The ceiling is only read on a row that could actually be DELIVERED.** `ConsecutiveAuto` is reset only
  by human interaction, so a killed agent carries a saturated one forever while its leftover escalation is
  still a candidate — standing the mode down over a row nothing could have sent, re-tripping after every
  re-enable (the latch clears on reload), and skipping the `autoAcceptOne` bookkeeping that would retire
  that row, so the livelock has no end. Hence the `live`/`autoAcceptParked` gate before the check, and
  `continue` rather than `break` after a stand-down so remaining candidates still register in
  `stillEligible` (a `break` hands them to `pruneAutoAcceptState`, silently resetting delivery budgets and
  absence counts). The two ceilings are tested SEPARATELY so the stand-down can name which tripped, and
  the per-minute one honours the window rollover.
- **The stand-down latches in memory FIRST, writes config second, off the select loop** — `UpdateConfig`
  nudges the daemon's own control socket, so an inline write blocks that loop on a round trip to itself.
  The latch stops the noticing sweep immediately, is checked inside `fspActive` so both entry points
  honour it through one gate, de-duplicates the write and the notification, and clears on reload.
- **The attribution is written at FINALIZE, and the finalize RETRY must carry it** —
  `autoAcceptNeedsFinalize` is `map[int64]bool` (id → was-FSP), not a set, or the flag is dropped on
  exactly the rows whose bookkeeping already failed once. Claim time is NOT an option:
  `ReclaimAbandonedAutoAccepts` would strand a true flag on a row nothing delivered. The store ORs the
  column, so a replay can only ever set it.
- **A generated task is screened TWICE, and the second one is the real gate.** The text is authored by the
  generator LLM AFTER the decision that raised the escalation, so no safety control has seen it — the
  operator's confirm was the gate, and this feature removes it. The screens run in the fork, before the
  seam, on the RENDERED `DeclaredTask.Prompt()` rather than the stored text (stored items keep line breaks
  as the literal two-character `\n`, which a line-anchored rule cannot match while the real newline
  reaching the pane can — screening the stored form fails OPEN). A hit reverts the claim and leaves the
  row escalated (FR-015). The daemon-side `generatedTaskUnsafe` can only render with the DEFAULT template
  since the target source is chosen inside the seam, so the seam is handed a `screen func(string) error`
  and calls it with the EXACT prompt immediately before the send, BEFORE the reservation.
  **Test trap:** the daemon's tests drive a FAKE seam, so only a `frontend` test proves the real path
  calls the callback.
- **A generated-task acceptance is the DAEMON's row to finalize** — `automated` makes
  `frontend.acceptGeneratedTask` skip BOTH `ResolveEscalation` and `InsertCorrection`. The seams
  (`Options.AcceptGeneratedTask` / `DisableFSP`) are optional function fields wired in `cmd/hap`, so
  `internal/daemon` still does not import `internal/frontend`; a nil seam returns the claim rather than
  stranding the row. **EVERY status check on that path has to know it**, including the cheap early-out
  above the claim (`audit.Status != "escalated"`) — missing THAT one is destructive: the error reaches
  `autoAcceptDeliveryFailed`, which burns an attempt per sweep and DISMISSES the escalation, so the
  feature deletes the very suggestions it exists to act on. It shipped green because the daemon tests wire
  a FAKE seam. The call runs INSIDE `WithAgentAutomation` like every other delivery.
- **EVERY generated-task hand-out gets a ledger row, the operator's included**
  (`recordTaskReservation`). A crash between the `[-]` and the send used to strand the item: the audit row
  is reclaimed at startup but the marker is not, so the retry reads it as taken, burns its budget, and the
  escalation is DISMISSED. The operator path recorded nothing on the ground that a human was present to
  read the error — **that premise died with the queued confirm**, since the send now happens on a machine
  they may not be sitting at. Accepted cost and likeliest support question: `agentsAwaitingHandout` allows
  ONE unconfirmed hand-out per agent, so a confirmed agent is withheld from the idle poll until it goes
  `working` or ages out at `staleHandoutTTL`. `TerminalID` comes from `liveAgentFor`, which is what tells
  a RECYCLED pane apart.
- **The mode is re-asked immediately before the claim** (`stillPermitted`); `WithAgentAutomation` covers
  the per-AGENT disable at delivery, not the global mode.

**The orchestrator is IGNORED unconditionally and CREATED only fail-closed** (`daemon/orchestrator.go`).
`full_self_prompting.orchestrator_agent_command` keeps an interactive claude session named `orchestrator` alive
while the mode is on; it watches `hap stream orchestrator`.

- **Disabled is not ignored.** A disabled agent is still captured, classified and audited on every event, so
  the orchestrator is filtered at every INGEST point (`handleTransition` after `noteRosterTransition`,
  `handleAttention`, `reconcileAttentionWith`, the sweep listing after `publishRoster`, the session-name flip
  pass) by `isOrchestrator`/`withoutOrchestrator`. A new pass over the agent listing must go through the filter.
- **The identity is `<state>/orchestrator.json`, never `agent_names.terminal_id`** — that column is rewritten on
  a recycled pane and carries name + disable onto the new tenant. Loaded before the first event; a recycled pane
  (same pane, different terminal) releases identity, name and disable (`observeOrchestrator`).
  - **The ensure pass observes FIRST, against the OLD identity.** A pane recycled while the daemon was down
    still carries the old name + disable; once a new orchestrator is recorded nothing compares that pane again,
    so an operator's agent stays disabled and its escalations are auto-dismissed as `agent_disabled`.
  - A just-opened pane is filtered by pane id alone while it starts (`provisionalOrchestrator`) — **in memory
    only**: persisted, a crash mid-start leaves an identity no recycled pane can contradict.
- **Creation fails closed**: mode on, not stood down, kill switch clear (read error = paused), argv[0] = claude
  (`domain.OrchestratorLaunch` — herdr starts by KIND), `ports.AgentLauncher` present. "Mode on" is the operator's
  switch, deliberately NOT `fspActive`'s preconditions. Off the loop, one pass at a time, ≤3 spawns/hour, doubling
  backoff. A failed `agent start` re-probes `AgentByName` before counting as a failure, so a readiness timeout
  never spawns a second session — and a real failure CLOSES the pane it opened, or every backoff step leaks a tab.
  An ADOPTED session is never briefed.
- **The mode is re-asked before each step that acts** (`orchestratorPermitted`: the start, the brief), from the
  LIVE config — `agent start` alone may take two minutes.
- **The brief never types into a modal**: it needs `ClaudeSessionFromPane` to prove an EMPTY composer, and is
  delivered without `WithAgentAutomation` on purpose (the agent is disabled by design). A fresh claude renders its
  transcript at the TOP of a tall pane, so a short read never sees a delivered brief (see
  `TestRealOrchestratorAgentStart`).

**Accepted limitation, not a bug:** a generated-task escalation is `idle`-typed, so its baseline salient is
unstructured pane-tail and Guard 3 usually answers `heldStillUnevaluable` — the row waits for the operator.

### Task sources and hand-outs

- **`enable_auto_send_task_when_idle` skips the LEARNING gates, never the safety ones.** A declared task
  from a flagged source resolves ahead of any learned noop precedence and bypasses the shadow-mode and
  confidence gates (`domain.Decide`, keyed on the `idleHandout` predicate — the VERIFIED classified
  situation plus the RESOLVED action, never a sweep-time flag). The flag is an operator instruction about a
  QUEUE; a learned action is an inference about a SCREEN, and every idle screen mints its own signature, so
  per-signature graduation means the feature never delivers unattended. The bypass sits AFTER the variance
  guard, rate guard and irreversibility heuristic and before nothing else; kill switch, never-auto,
  per-agent disable and the optional pre-delivery LLM review all still apply at delivery.
- **An unattended hand-out is only "delivered" once the agent works.** `agent send` proves herdr took the
  keystrokes, not that the agent acted, so the `[-]` is recorded in `task_reservations` and confirmed only
  by a `working` transition; `reclaimStrandedTasks` returns an unconfirmed item to `[ ]` once its agent is
  parked again past `reclaimGrace`. Four bounds: the daemon releases ONLY a `[-]` it holds a ledger row for;
  **one unconfirmed hand-out per agent** (`agentsAwaitingHandout`), since confirmation is per-agent and a
  second hand-out lets one resumption confirm — and so strand — the untaken first; confirm and reclaim both
  compare `terminal_id`, because an agent id IS a recycled pane id; and an item handed out `maxTaskHandouts`
  times without ever being started is left `[-]` and escalated rather than resent forever.
- **A pending escalation never benches an agent from the idle poll** — `eligibleIdleAgents` has no
  escalation gate, deliberately: gating on one deadlocks the feature against itself, since a pending task is
  what raises `noop_vs_pending_tasks`, which then blocks the poll that would deliver it. **Do not reach for
  the audit `Trigger` to tell "the poll's own" escalations apart either** — `daemon.trigger` derives it from
  `tr.AutoIdleSend`, so EVERY escalation on a poll-driven episode is stamped `auto-idle-send:`, the common
  shape for exactly the parked agents this is for. **The bound on an undeliverable task is per ITEM, not per
  agent**: a failed send rolls back to `[ ]` and records NO reservation, so nothing can age it —
  `deliverAutonomousClaimed` counts the attempt (`RecordTaskHandoutAttempt`) and at the cap leaves it `[-]`
  and escalates. Since the poll ignores `episodeHandled` on purpose, `Daemon.pollRedrive` widens the
  interval (1, 2, 4 … capped at 15m) for an agent whose episodes keep not sending — a delay, never a bench.
- **An inherited task-source provider is a live link, never a snapshot.** An empty per-source `provider` IS
  the inheritance and must NEVER be materialized: `normalizeTaskSources` runs on **Load AND Save**, so
  filling it in "like we do for `max_tasks`" stamps the current default onto every existing source the first
  time any surface writes config — and the operator's later `hap config set task_source_provider.provider`
  then moves nothing, silently, for every install that already had a config. It fails no obvious test.
  Misconfiguration is detected at USE time (`ValidateResolvedProvider`), never at `Load`, and never coerced
  — coercing an unrecognized provider to `local_fs` resolves gist-shaped names against the daemon's cwd and
  CREATES local files while the operator believes their lists are remote. A missing `gist_id` is
  deliberately not a write-time rule either: it would make the keys order-dependent to set.
  - **The TOP-LEVEL key is the opposite: it IS materialized, and that is what made flipping its default
    safe.** `Default()` names `sqlite` and `Load` pins `local_fs` the moment the config FILE exists — before
    the decode, so an explicit key still wins and EVERY return carries the pin, including the two error
    paths and the `AutoAccept.validate()` branch that keeps the decoded config. A post-decode pin misses
    that third one silently, so a typo in the file would move an install's storage backend.
    `ResolveProvider`'s literal fallback stays `local_fs` (reached only by an in-memory `Config{}`), and
    `AnyNonDefaultProvider` asks whether the config is MIXED rather than "anything but local_fs", since
    there are now TWO postures an operator reaches without touching the setting.
  - **Test trap:** `internal/cli` and `internal/frontend` fixtures whose list is a FILE now declare it
    (`localFSApp`/`localFSCfg`) rather than riding on the default — a suite where every test runs on a local
    file, where a locator IS a path, is the single reason the locator-is-not-a-path bugs kept shipping
    green. `testApp` deliberately stays on the DEFAULT.
- **A task-list locator is never handled as a FILE outside the local backend.** `taskfile.Mutate`,
  `MutateWithin` and `WriteFileAtomic` take a path; a locator is not one. Under a remote provider it is a
  `gist://<id>/<file>` URI that `os.Stat` can only fail on, and the failure reads like a missing file rather
  than like code looking in the wrong place. Every checklist mutation goes through the STORE
  (`frontend.mutateList`/`mutateTask`, `daemon.mutateTaskList`), and there is deliberately NO local
  `mutateTaskFile` alias to reach for; `internal/taskstore/local` is the one exemption. **The hazard is that
  the local twin of a store method passes every test** — three shipped: a send-time reservation left on
  `taskfile.Mutate` created the list, registered the source, CLAIMED the escalation and died with nothing
  sent; `pickAppendTarget`'s `os.ReadFile` silently degraded from "the source with pending work" to "the
  first source"; and the bootstrap exclusion compared absolutized paths, so an agent's own list read as
  external and the append path duplicated every task un-numbered.
  - Comparison is on canonical LOCATORS (`isBootstrapList`) — **plus a declared path**, not optional: a
    DERIVED source resolves to `DerivedFileName(agent)`, byte-identical to what the bootstrap registers, so
    locator equality alone excludes the most idiomatic gist setup and `addTaskSourceIfAbsent` then refuses
    to register a second source, failing every retry after the tasks were written. Never compare the
    declared spelling ALONE either: the same file name in a different gist is a different list. The
    bootstrap locator's own resolution error is DEFERRED past the append early-return, so a misconfigured
    remote default beside a healthy `local_fs` source still works.
  - Two guards cover different halves: `TestOnlyTheLocalBackendTouchesATaskListAsAFile` bans the path-taking
    MUTATORS by construction (matched on the selector, so a function value is caught and a dot-import of
    `taskfile` refused), while the READ half needs behavior — hence `App.TaskStoreFor`, the test-only seam
    standing a remote backend up in-process. Changes here belong in `remote_confirm_test.go`.
  - The reservation rollback takes `context.WithoutCancel`: a remote store uses the caller's ctx, and the
    likeliest cause of the failed send it undoes — the operator quitting — is the same cancellation that
    would abort the release and strand the item at `[-]`.
- **A task-list locator is canonicalized in exactly one place** — `tasklocator.Canonical`. A second copy
  forgetting that a scheme'd locator returns verbatim fails SILENTLY: `filepath.Abs` does not error, it
  returns `<cwd>/gist:/id/file`, and each hap process has a different cwd.
- **A task list is never created BLANK.** GitHub refuses `""`, `"\n"` and `" "` alike with
  `422 Validation Failed {Resource:Gist Field:files Code:missing_field}` — a blank body reads as "this entry
  carries no file", the entry is dropped, and the request fails for an empty `files` map, so the error names
  `files` and never the content. `newListHeader` is the ONE seed every create-on-demand path passes to
  `ensureList`, enforced in THREE places: `frontend.ensureList` refuses a blank seed for EVERY backend (a
  local file takes one happily, so guarding only where it breaks means the next caller is green through the
  whole unit suite and fails for the first `github_gist` operator — exactly how the generated-task confirm
  shipped seeding `""`); `ports.EnsureCreator` states it as the interface contract; and `gist.Store.put`
  refuses with `ErrBlankContent` on EVERY write path. Only the CREATE is closed.
  **Test trap:** the unit suite could not catch any of this until `fakeGist` was taught to answer a blank
  write with GitHub's real 422.

### Store, nodes and sync

- **The store is node-scoped, and the Turso engine is daemon-owned, gated and never cancelled.** Several
  machines may share one database and a herdr pane id repeats on every one, so it is never an identity
  alone. Node-owned rows carry `node_id` (`store.LoadNodeID`); machine-local natural keys are composite
  (`agent_names(node_id, agent_id)` with `UNIQUE(node_id, name)`, `agent_rate`, `error_retries`,
  `task_handouts`, `agent_roster`, `herdr_locations`, `roster_meta`); INTEGER keys get node bits under turso
  (`store.TimeOrderedIDs` via `s.nextID()` — NULL under sqlite so AUTOINCREMENT still assigns). Every
  OPERATIONAL statement filters `node_id = self`; FLEET reads span nodes and return `node_id`.
  `TestEveryNodeOwnedStatementIsNodeScoped` enforces it by AST walk with an exemption map that must stay
  live; the store suite runs THREE times (`HAP_STORE_TEST_MODE=sqlite|proxy|turso`). Two-node sync tests in
  `internal/store/turso` need `tursodb` on PATH (skip otherwise); `HAP_TURSO_TEST_URL` +
  `HAP_TURSO_TEST_TOKEN` point them at a REAL database instead — its hap tables must start empty, so run
  them one at a time and wipe between.
  - `hasOpenEscalation` asks the store, never filters the fleet queue by agent id in Go — that would let
    another machine's pane `1` block this one's reconcile.
  - Under turso only the daemon opens the file (the sync engine allows one process); other processes get a
    `database/sql` driver over `<state>/store.sock` (`internal/store/sqlbridge`), lazily dialled so
    `hap config` works with no daemon.
  - The adapter gates the SDK (statements read-lock, Push/Pull/Checkpoint write-lock, transactions hold the
    lock, rows returned EAGERLY) over a FIXED pre-warmed pool, with sync ops on a background context —
    verified: unguarded they flood `database is locked`, and a Push cancelled mid-flight hangs the engine
    for good. Every pooled connection gets `turso.PageCacheKiB` of page cache — at warm-up, and again whenever a
    bridge session takes a connection (`sqlbridge.Executor.SetConnInit`), which is what reaches one database/sql
    opened to replace a discarded one; a refusal is logged, never fatal. The engine default is
    2000 KiB PER CONNECTION, kept for the daemon's life across the whole pool, duplicating the kernel's page
    cache. The engine clamps anything under 200 pages up to 200 and reads it back as `200`, so the constant
    sits at that floor.
  - **Schema DDL on a shared database is issued only by the SCHEMA LEASE holder**
    (`turso.PrepareSharedSchema` / `AcquireSchemaLease`), which must be RE-PROVEN between migration steps,
    failing closed with `ErrSchemaLeaseLost` — a background renewal alone is starved by a step's own write
    lock. Two identical ALTERs wedge the loser SILENTLY, and elapsed time is never ownership: a node that
    cannot establish the lease fails closed rather than migrating blind.
  - `fleetRun` runs every sync op off the loop and waits for it OR shutdown; `turso.DB.Close` waits a
    bounded time and refuses to close underneath an in-flight op.
  - **A front end polls a change token, not the data** (`Store.Revision` → `ports.RevisionReporter`,
    `frontend.App.ChangeKey`, `tui.Model.poll`). Under turso it is the executor's counter
    (`sqlbridge.Executor.Revision`, a `rev` request over a POOLED connection), bumped AFTER every committed
    write and every pull that `changed` — so sampling it before a read can only cost an extra refresh, never
    miss one — and prefixed with an epoch because a restarted daemon counts from zero. Under sqlite it is
    the file's and WAL's size+mtime. The key adds config.toml and local checklist files; a **gist** source
    answers "cannot tell" (no local trace), as does a store without the port, and both keep the old
    re-read-every-tick behaviour. What a refresh derives from the CLOCK is re-read on `refreshBackstop`, and
    an open agent detail always re-reads (its permission mode comes from the PANE). The unconditional 2s
    re-read was the TUI's whole idle cost and serving it most of the daemon's while one was open.
  - Config never enters the database. Front ends draw ids from the daemon with NO local fallback: two
    processes minting locally in one millisecond collide, so an insert with no id fails with the reason
    (`failedID`).
- **A periodic write is CONDITIONAL, or it is a leak** — every store write arms a **2s-debounced turso
  push**, so a writer firing more often than that never coalesces: one tick, one row, one push to Turso
  Cloud, forever, on an idle install. Two writers were that shape:
  - **`PublishRoster` compares before it writes** (`rosterRowUnchanged`), which **MIRRORS
    `upsertRosterRow`'s CASE arms field for field — the two must change together**, or a field added to the
    UPDATE alone silently stops being published. Three exclusions, each reasoned: `seen_at` advances by
    construction (including it makes every row dirty — the whole bug), `cwd`/`cwd_read_at` are never written
    by a publish, and `gone_at` is COMPARED rather than excluded so a returning agent is republished. A
    **recycled** id is never skipped — its row was DELETEd earlier in the same transaction, so `existing`
    describes a row that no longer exists, and the gate is on `recycledIDs` structurally rather than on the
    coincidence that `terminal_id` is in the compare set. Roster FRESHNESS is `roster_meta.published_at`
    (`domain.RosterFresh`), never a per-row `seen_at` — and the stamp is itself a write, so a publish that
    changed nothing re-stamps only once it is `domain.RosterRestampAfter` old, a constant bounded ABOVE by
    the one-minute sweep so an unwatched herd's every sweep still stamps. A changed row stamps at once.
  - **Only a transaction that WROTE reports a write** (`sqlbridge.Session.txWrote`) — COMMIT used to
    report unconditionally, so every read-only transaction armed a push. Any successful Exec counts,
    whatever it affected: DDL reports 0 rows and must still reach the other nodes.
  - **`domain.NodeHeartbeat` is a SEPARATE constant from `daemonhealth.HeartbeatInterval` — deliberately
    slower, but BOUNDED ABOVE by `daemon.actionStaleAfter`.** The file answers "is this daemon hung" and
    must be fast; the row answers "is this machine still out there", and no reader asks with more precision
    than `domain.NodeStale` (three beats). `maybeUpsertNode` throttles it and its FIRST call always writes,
    which is why `Run`'s startup call goes THROUGH the throttle rather than around it. **The ceiling is why
    it is 30s and not the minute the write reduction alone would prefer**: three beats is also the gate
    `requireLiveDaemonFor` refuses a remote confirm on, while a confirm vouches for a SCREEN whose
    deliverable life is `actionStaleAfter` — so a confirm the gate accepts must still be one the daemon can
    honour, and a minute inverts that silently. (`domain` cannot import `daemon`, so a test is what makes
    the ceiling enforceable.)
- **A wedged sync engine is REPAIRED once and SAID always, and the two halves have opposite defaults.** A
  failing `Push`/`Pull` used to be `logging.Guard` + Warn + `return nil`: the daemon kept running, the herd
  looked quiet, and every fleet read this node served was silently half a fleet short. Observed live (macOS,
  hap 0.9.4) as a TLS handshake failing every tick with `last pull never, last push never` while the TUI
  showed six escalations and named them all local.
  - **The counter spans BOTH directions and the anchor is the last SUCCESS** — a node that pulls fine and
    cannot push is just as isolated. Isolation measures from `max(LastPullAt, LastPushAt)`, falling back to
    `FirstFailureAt`, the ONLY anchor a daemon that has never synced has, which is the exact shape the
    incident was reported in. `FleetSyncIsolatedAfter` is a CLOCK, not a boolean on `LastError`: a fresh
    failure is `DaemonWarn` and only five minutes without a success is `DaemonError`. The banner leads with
    the CONSEQUENCE ("this machine is NOT exchanging rows with the other nodes"), never the TLS text.
  - **The automatic restart is gated on the fault being PROCESS-LOCAL, and unknown means NO**
    (`domain.SyncFailureProcessLocal`, remote shapes checked FIRST so a TLS error nested in a dial timeout
    reads as the timeout). Restarting on a remote that is merely down costs the herd its in-flight captures
    and consults every cooldown, forever, and buys nothing. Both bounds must clear
    (`fleetRecoveryMinFailures` AND `fleetRecoveryMinOutage`).
  - **It spawns `--restart`, NOT the `--ensure` the upgrade handoff uses, and this is the trap.**
    `EnsureFresh` does NOTHING when the running holder already matches the version and path it would start
    — precisely a restart as the SAME binary — so an `--ensure` successor bows out and a daemon that then
    stepped aside leaves the herd unmonitored. `checkFleetSyncWedged` latches `fleetRecoveryOrdered` and
    keeps running rather than taking `handedOff`.
  - **The latch is released by EVIDENCE, not by time** — a marker file written BEFORE the spawn (the
    successor may read it first) and deleted by the first successful sync. An unwritable marker REFUSES the
    restart, because without it an unfixable fault becomes a restart loop abandoning in-flight work every
    cooldown, strictly worse than the isolation; a failed spawn clears it.
  - **An unbootstrapped node is a THIRD state, not a degraded one.** `openTurso` retries the first bootstrap
    forever and `daemonlock.Acquire` runs BEFORE it, so a wrong URL or rejected token leaves `hap status`
    reporting a running daemon that has not begun monitoring anything — worse than isolation, which at least
    answers for its own herd. Its own banner and remedy, on the same clock; `openTurso` records
    `FirstFailureAt` because nothing else on that path has one.
  - `internal/fdprobe` is instrumentation, not a control — fd exhaustion was the leading explanation and was
    never MEASURED. `OpenKnown` is separate from `Open` because macOS has no `/proc/self/fd` and rendering
    its zero as "0 open" reports the opposite of what happened; `Exhausted` is a real syscall and the only
    field that is proof.
- **Retention has TWO windows, and the exemptions are the safety control.** `PruneAuditExcerpts` blanks one
  COLUMN; `PruneAgedRows` deletes finished bookkeeping ROWS (default 30 days). Both share the daemon's one
  daily throttle, taken before either config is read so switching one off cannot change the other's cadence.
  - **`agent_roster` is swept only because `agent_roster_tombstones` exists, and the trap it answers is still
    live**: a retired row was itself the resurrection guard. `upsertRosterRow`'s INSERT arm hardcodes
    `gone_at = 0` and only its ON CONFLICT arm honours `authoritative`, so with nothing recording the
    retirement a non-authoritative EVENT takes the insert path and revives an agent herdr no longer reports
    — `LiveRoster` then hands a dead agent to the idle poll and `hap task send` until the next
    authoritative publish re-retires it. (A plain delete was
    tried, #395, and backed out, #398.) Four bounds:
    - **The prune requires the tombstone, per row.** A retired row without one (legacy or damaged database)
      is the last guard left, so it is KEPT; `migrate` re-derives missing tombstones on EVERY open, since the
      wide row is the only place that information still exists.
    - **Discriminated by TERMINAL id, and storing it without reading it is the silent half of the bug** —
      once the wide row is gone the tombstone is the only record of which terminal was retired. A DIFFERENT,
      non-empty terminal is a genuinely new agent and is admitted; an empty terminal on EITHER side blocks
      ("unobserved is never evidence").
    - **Admitting DELETES the tombstone**, or a survivor keeps the OLD terminal at this agent's own
      retirement and the comparison silently stops discriminating.
    - **The tombstone is permanent, deliberately** — "this terminal on this pane is retired" is true forever,
      and a grace period needs a bound on transition staleness the protocol does not offer.

    `hap gc` does not reclaim roster rows because it does not call `PruneAgedRows` at all — pre-existing;
    the daily sweep is what bounds the table.
  - **`audit_log` and `decisions` are never swept** — the row survives its blanked column so `hap audit`
    history stays complete, and `decisions` feeds `CountDecisionsForSignature`, so deleting from it changes
    LEARNED BEHAVIOUR rather than reclaiming space.
  - Every other exclusion is a row some path still acts on, never one that merely looks recent: a
    non-terminal `agent_actions` row is the control queue (and even a terminal one is what
    `AwaitAgentAction` returns as `Result`/`Error` — the ONLY way the surface that queued it learns whether
    it landed); `pending` `llm_requests`/`llm_decisions` rows are the retry guard and an un-re-gated
    decision; unprocessed `corrections`/`llm_retries` rows are queued work. `corrections` additionally needs
    `NOT EXISTS` over `agent_actions.correction_id` — that reference has no foreign key behind it and is
    what makes `UnprocessedCorrections` withhold a correction whose delivery is still queued.
  - **An unconfirmed `task_reservations` row survives at any age** — it is what `reclaimStrandedTasks` needs
    to return an item to `[ ]`, and a `[-]` with no ledger row is treated as somebody else's.
  - **The newest `kill_events` row survives PER SCOPE, not per node.** The table carries a SECOND stream (the
    FSP toggles `recordFSPToggle` writes, including the daemon's ceiling stand-down), so a survivor guard
    keyed on `MAX(id)` alone deletes a standing global PAUSE the moment any newer FSP row exists —
    `KillStateActive` reads false and the herd resumes with nothing logged. **Test trap:** this shipped green
    because the first version of the test seeded only `global` rows.
  - **A finished consult's payloads go on their OWN grace (`LLMPayloadGrace`), never at the status
    transition** — neither `GetLLMRequest` nor `LLMDecisionByRequest` filters on status, so
    `mcpserver.resolveRequest` serving an explicit `request_id` would be handed an EMPTY context. Separate
    from the operator's window precisely because that one may be 0.
  - **The cutoff is FLOORED at `RowRetentionFloor`, because 0 is a supported setting** — otherwise a terminal
    `agent_actions` row is deletable in the same second it is written, while `AwaitAgentAction` is still
    polling it for the only outcome signal it can get. A **separate constant** from `LLMPayloadGrace` at the
    same value, the way `PruneAuditExcerpts` has `AuditExcerptDedupMargin`: that one bounds a COLUMN blank
    against a live reader, this one a ROW delete against a poller.
  - **Every statement is issued at its OWN call site rather than from a table of queries** —
    `TestEveryNodeOwnedStatementIsNodeScoped` flattens a CALL's SQL argument, so a query reached through a
    struct field flattens to `" ? "` and the whole sweep falls outside the guard, silently, in the one file
    where an unscoped DELETE does the most damage. Hoisting SQL into package consts does NOT fix that; only a
    direct literal at the call site does.

### agy (Antigravity CLI)

**herdr never reports an agy modal as blocked, so every agy form is recognized structurally and
PARKED at idle/done** (`internal/domain/agy.go`, corpus and design in
`docs/designer/agy-support.md`). Each parser requires the form's own anchors AND its key-hint
line at the true bottom of the capture — agy renders inline, so its whole transcript is in every
read and an earlier form is always somewhere above.

- **A reply to an agy approval or question is KEYS, never submitted text**
  (`domain.AgyFormSituation` → `mcqdeliver.Agy`). Every generic path types the reply and then
  Enter, and agy commits on the digit alone, so that Enter answers the NEXT screen (on a
  two-question form, option 1 of question 2, unseen). All four send paths route agy forms to the
  keyed deliverer — `daemon.act` (ahead of the action-review rewrite), the LLM promotion (on the
  RE-CLASSIFIED type), `deliver.Deliver` (the operator's `--send` and auto-accept) — and the
  action-review outcome refuses one outright. A new send path must ask `AgyFormSituation` too. The
  deliverer's rules are all load-bearing:
  - **one key, then re-read; a key is never pressed twice** — a repeated digit answers whatever
    the first one opened. Numbered forms get the digit, the review panel `y`/`n` (never `shift+a`),
    the trust prompt arrows verified row by row and only then Enter;
  - **the live form must be the DECIDED one** (`AgyForm.SameAs` against the decision's excerpt) —
    question 2 can offer the same labels as question 1;
  - **a question's Write-in row is refused as a verdict** (`domain.ErrAgyNotAnswerable` →
    `deliver.ErrReplyWithheld`, which auto-accept maps to `errOutboundRefused` — without it the
    refusal burns the attempt budget and DISMISSES the row);
  - **every verified answer re-captures the pane** (`recaptureAfterAgyAnswer`, from the keyed-form
    bodies AND `autoAcceptDeliver`): agy draws the next question IN PLACE with no status change,
    so nothing else would ever look at it — the form stalled after question 1, silently.
- **Anything else typed into agy needs a proven EMPTY composer** (`domain.AgyComposerReady`,
  `daemon.agyComposerRefusal`): herdr says idle under every agy modal, so a status check lets a
  hand-out land in a standing approval, a picker, the survey or the operator's draft. Asked by
  `deliverAutonomousClaimed`, the LLM promotion, `deliver.Deliver`, `refuseIfAgentBusy` (with the
  stale marker, so the TUI offers to queue instead), `requireIdleForHandout` and
  `actionTaskSendHost.Send`. A new hand-out path must ask it too. **Test trap:** the daemon's
  `fakeHerdr` has no `SendToAgent`, which is why the proof lives at the call sites and not in the
  herdr adapter — there it would be invisible to every daemon test.
- **Never set `MCQKind`/`AnswerCount` on an agy situation, and keep `ParseMCQForm` false for agy**:
  each agy question is its own situation (the next one's options are not rendered until this one is
  answered), and `EffectiveAnswerCount() > 1` routes into `sweepFrames` and `mcqdeliver.ClaudeTabs`,
  which press Right/Left into the pane BEFORE they refuse.
- Setup (sign-in, terms) and operator UI (pickers, panels, slash popup, the Tab-amend field, the
  survey) classify **unclassifiable**, ahead of every rule including the operator's — no consult,
  no suggestion, no keystroke. Enter on the terms screen flips the data-sharing consent.
- The agy approval `PermissionVerb` carries the command UNMASKED on purpose:
  `IrreversibleScanContent` reads it raw alongside the 40-line pane tail, and `MaskVolatile` turns
  `of=/dev/sda` into `of=<path>` — the tail usually carries the raw command too, but a long
  wrapped command can push it out, and the verb is then the only raw copy.
- **Modes (`default → acceptEdits → plan`) are read loosely and pressed strictly.** The READ
  (`AgyAgentMode`) is not gated on an empty composer — a working agy or one holding a draft still
  paints its mode, and `hap agents` should show it — but the PRESS gate (`ComposerReadyForMode`) is
  `AgyComposerReady`. Two things the pane cannot say: `--dangerously-skip-permissions` paints no
  indicator (such an agent reads `default` at launch), and a status bar with no model segment carries
  no mode (UNKNOWN, never default). agy's `default` is its MOST restrictive mode, codex's its least.
- **Session ids come from the print-mode JSON envelope only** (`llm.ExtractSessionID`): herdr reports
  no `agent_session` for agy, and `--conversation` resumes an existing id rather than naming a new one,
  so nothing is injected. The id is read from a line that DECODES as the envelope, never by searching
  for the key, so an id quoted in the response (escaped inside the envelope) is never taken.

### Claude session-name sync

**A Claude CONVERSATION name is read only from a proven composer, and its ABSENCE is never evidence.**
`[agents] sync_claude_session_name` (off by default) keeps an agent's hap name and the name `/rename` paints
in Claude's composer byte-identical: a named session is folded, adopted, and pushed BACK when the fold or a
collision changed it; an unnamed one is sent `/rename <hap name>`.

- **It is not the terminal title** — `terminal_title_stripped` carries Claude's churning conversation
  SUMMARY, so adopting it renames every agent after a sentence that changes on its own (Claude Code 2.1.252).
- **"No composer" is UNKNOWN, never "unnamed"** — the classification read is a consuming delta that routinely
  shows no footer, and the push direction reads "unnamed" as its TRIGGER, so the alternative overwrites an
  operator's chosen name.
- **The push is a DELIVERY**: `acquirePane`, kill switch + per-agent disable re-asked inside the goroutine,
  never-auto over the exact text, a `--source visible` re-read before AND after the send, a proven-EMPTY
  composer (`ClaudeComposerReady` proves the sandwich, not that it is blank), a ceiling per (agent, terminal,
  name), and `d.spawn` so shutdown drains it.
- **QUIESCENCE is asked twice, and the second time against LIVE state.** Both questions — parked
  (`sessionRenameParked`: `idle`/`done`; `blocked` is a modal where Enter is rebound, and an empty status
  fails closed) and `ComposerEmpty` — are asked at the TOP of `applyClaudeSession`, the one seam all three
  entry points share, gating BOTH directions including the store-only adopt. They are asked again inside
  `pushSessionRename` from `liveAgentFor`, NOT `tr.Status`: the capture's status is seconds old on the
  attention path and a whole pass old on the others, and claude QUEUES input while it works rather than
  refusing it. A failed listing refuses — "we could not ask" is not "it is idle". The tenancy compare
  (`recycledSince`) fails OPEN on an unknown id, because event-socket transitions carry no `terminal_id` and
  a strict compare would refuse every production rename. Order is deliberate: status first (cheaper, skips
  the read), composer proof LAST, because typing changes on one keypress while status changes at a turn
  boundary.
- **A just-parked agent is not a quiet one** (`sessionRenameSettle`, via `sessionSyncReady`). The complaint
  this feature earned is a rename typed into a session the operator opened seconds ago: the composer is empty
  because they have not typed the FIRST character yet, so both quiescence checks pass and the push races
  their first keypress. No re-read closes a sub-second race; waiting does. The evidence is `d.idleSince`, and
  an ABSENT or foreign mark is UNSETTLED — exactly the state a brand-new agent is in.
  - **It gates ADOPTION too**, though adoption types nothing: what it buys is that the two names are never
    knowingly left disagreeing, which is the CHARACTER-IDENTICAL contract this feature exists to hold.
    Accepted cost: an escalation inside that window calls the agent by its generated name.
  - **It is asked in exactly TWO places, and a third copy is a hazard rather than defence in depth.**
    `startSessionRename` deliberately carries none — the shared gate already answered over the capture and
    `pushSessionRename` re-asks against LIVE state, which is strictly stronger. A copy over the stale `tr`
    could only agree with the gate that just ran, and it made the mutation deleting the REAL check pass.
  - **The live re-check asks "parked LONG ENOUGH", not just "parked"** — an agent can park AGAIN in the gap
    the goroutine spends on herdr, a NEW spell the pre-spawn check knew nothing about.
  - **The constant is not the knob it looks like** — `d.idleSince` is written only by the 60s sweep and a
    deferral's first backoff step is also 60s, so the effective wait is ~1–2 minutes whatever
    `sessionRenameSettle` says. Lowering it changes almost nothing; 0 removes the gate.
- **A refusal DEFERS; it never burns a push.** `maxSessionRenamePushes` bounds KEYSTROKES at a pane that never
  takes the rename; `maxSessionSyncDeferrals` bounds READS at a pane that is never ready. Conflating them is
  destructive: every refusal used to burn one of the three, so an operator who was mid-draft three times
  running permanently disabled their own rename — the exact person the gates are for. `pushSessionRename`
  returns `typed bool` and the release refunds by DECREMENT (never by writing back a snapshot, which hands a
  concurrent claim its budget too). The one branch that must arm NOTHING is a send that happened but did not
  verify: a deferral there turns the ceiling into "three pushes per interval, forever".
- **The retry is the sweep's, not a timer's** — `sessionSyncDeferred` is a `pollRedrive`-shaped map re-examined
  off the existing 1-minute ticker at 1→2→4→8→15 minutes, and `nextAt` is load-bearing because the ticker has
  no phase relationship to when a deferral was armed. The pass shares `sessionSyncPassRunning` with the flip
  pass so two passes never walk the herd typing at once — which means a false→true flip arriving while a retry
  holds the latch MUST be coalesced (`sessionSyncFlipPending`) rather than dropped: nothing else re-runs the
  one-shot live-herd sync, and the retry pass only visits agents that already carry a deferral. It is handed
  BOTH slices: the whole listing is what the map is PRUNED against (an agent withheld from `rest` has not
  vanished), while only `rest` may be touched.
- **The `!ok` capture arms a retry, and the aligned fast path is what makes that affordable.** "No composer in
  this capture" is the NORMAL state for a quiescent pane and the state the operator's own scenario sits in, so
  leaving it to the next capture leaves the feature with no retry at all for the case it exists for. That arms
  one deferral per claude agent, which the `sess.Name == agentName` fast path ABOVE the gate clears on the
  first retry. Without the fast path, every settled agent that happens to be mid-turn arms a retry instead.
- **`NormalizeAgentName` must stay a FIXED POINT**, or the pushed name is re-folded on the next capture and
  the two trade spellings forever. Same for `SuffixedAgentName`; collisions are idempotent via
  `domain.AgentNameDerivedFrom`. An identical pair must cost no pane read — the at-send screen also refuses
  the redundant push, so only a read COUNT catches its removal.
- **Turning the key ON drives its own one-shot pass, because a config change re-captures NOTHING.** Neither
  `reloadWith` nor its `reconcileAttention` schedules a capture for an already-parked agent, so a flip on a
  settled herd did nothing until each agent next went working→parked. `syncClaudeSessionNamesNow` walks the
  live agents once instead, and four bounds are load-bearing: it reads `--source visible`, never `ReadPane`'s
  consuming delta (which would swallow the delta a pending classification capture is about to take, and is
  also the only reason the flip sees a composer at all); clearing `episodeHandled` is NOT the alternative,
  since that re-drives the whole herd through classify→decide→act, raising escalations and spending LLM
  consults for a naming feature; the trigger is gated on `!first`, because `reloadWith` also runs inside
  `New()` (so a daemon STARTED with the key on keeps the pre-fix behaviour for agents whose escalation row
  survived the restart); and the latch is released by the goroutine's defer AND by hand when `spawn` refuses,
  or one shutdown-race flip disables the pass for the process. Both entry points share `applyClaudeSession`,
  so a gate added to either is added to both.
- **Test traps:** every gate fails CLOSED, so a push case that forgets `parkedAndSettled` (pin the listing AND
  backdate `d.idleSince`) passes for the wrong reason — hence the ceiling test asserts EXACTLY the ceiling
  rather than "no more than". And the flip tests wrap the fake so the composer is visible ONLY through
  `--source visible`; without that the capture path could produce the same rename and none would discriminate.

### Semantic matching

- **A knowledge rebuild runs under `tightGC`** (`daemon/memory.go`) — loading every signature and building a
  fresh index is the daemon's one large allocation burst (~40MB against a live heap of a few MB), and at the
  default GC target it set the daemon's high-water mark and held ~80MB resident for its first minute. The
  target is lowered only for the burst and the heap handed back at its end; a process-wide GOGC=25 was measured
  at roughly double the idle daemon's CPU. It only ever LOWERS (an operator's tighter GOGC stays), overlapping
  rebuilds nest, and GOGC=off is not touched at all.
- **Semantic matching degrades, never blocks** — situations resolve via embedding + vector search over the
  MASKED salient (`daemon.resolveSignature`, `internal/match`, `internal/embedder`), falling back to
  normalized BM25, then exact hash. `SignatureResult.Raw` is the never-remapped content hash (the LLM drift
  check depends on it); `signature_embeddings` is the source of truth and the bleve index under
  `<state>/match-index` is a disposable cache (mem-only scorch does NOT serve KNN — keep it disk-backed).
  Embed calls are stall-guarded and latch a degraded mode after 5 consecutive failures.
- **Attention events are delay-captured** — the classification read waits `[[capture_delay]]` (10s on an
  agent's first event, 2000ms after) via a per-pane `time.AfterFunc`, so the agent TUI has painted and bursts
  coalesce (latest wins, one capture per burst). Daemon tests inherit a 1ms wildcard rule from the harness.
- **Learned signatures are FLEET-WIDE, and the embedding model's id is the only thing that scopes them.**
  `signatures`, `signature_embeddings` and `signature_snapshots` carry NO `node_id`, and `decisions` is
  deliberately absent from `nodeScopedTables` so rules graduate on the fleet's evidence. A peer's rule becomes
  matchable HERE through `fleetPull` → `RefreshKnowledge`, which rebuilds the whole bleve index from the store
  rather than adding rows — which is also what makes a rule DELETED elsewhere disappear here. **Do not add a
  `node_id` to any of them.**
  - **`embedder.ModelIDFor` is therefore a fleet-wide identity, and both halves are load-bearing.** It digests
    the model FILE. `filepath.Base` failed BOTH ways at once, silently — *not unique enough*: two different
    384-dim models installed as `model.gguf` shared an id, so `Reconcile` KEPT foreign vectors and cosine
    compared across unrelated models, which no downstream equality filter can catch because the strings agree;
    *not stable enough*: a renamed copy of the bundled model gave one node a different id for the same model,
    so each node read the other's rows as stale and re-embedded them, an unbounded rewrite ping-pong through
    Turso Cloud plus a permanent "N rules need re-compute" nag. Unreadable falls back to the base name, never
    `""` (an empty id compares unequal to every row, so drift could never clear) — and that fallback is NOT
    cached, or an install whose model arrives later keeps the legacy scheme for the process's life.
  - **The id is hashed once per MACHINE, not per process** (`embedder.SetModelIDCacheDir`, `<state>/model-ids.json`,
    set by every hap process in `cmd/hap`): streaming the ~25 MB model through SHA-256 cost every
    `hap status` / `hap agents` ~50ms on an idle devbox and several hundred ms on a busy one. An entry is trusted
    only while size, mtime, inode, device AND ctime match what was hashed — ctime is the one fact user space cannot
    set, so a model rewritten in place with its mtime restored (`cp -p`, `touch -r`) is still re-hashed at the next
    process start; an id is persisted only if the file's facts held still across the hash. The in-process cache
    still holds for the process's life; `rm <state>/model-ids.json` forces a re-hash. Best-effort: an unreadable
    cache just hashes.
  - **Anything comparing a stored row's model must resolve the id the SAME way** — `frontend.embeddingDrift`
    calls `embedder.ModelIDFor` rather than taking the base name; the two drifting apart is silent and
    permanent. `EmbeddingDrift.ModelName` is DISPLAY only; `ModelID` is the comparison key.
  - **Known and NOT fixed:** a fleet whose nodes run genuinely different models, or disagree on
    `min_salient_chars`, still ping-pongs — `Reconcile` rewrites peers' rows on every pull. The vector is
    stored per SIGNATURE, not per (signature, model), so the alternative would confine cosine to rows each
    node embedded itself. A uniform fleet is unaffected either way.
- **A short PANE-TAIL salient is never embedded — on EITHER side of the comparison.** Below
  `embedding.min_salient_chars` (default 100, on the masked salient) matching uses BM25. **STRUCTURED salients
  are exempt at any length, and that exemption is load-bearing**: they are short by construction
  (`permission:proceed | options:no;yes` is 35 chars), so a floor over them switches cosine off for every
  approval, choice and error rule — the paraphrase matching the feature exists for. **If every pre-existing
  semantic test needs a lowered floor to pass, the floor's scope is wrong** — that was the tell the first
  time. The reason for the floor: sentence embeddings are not discriminative on a few generic tokens, so any
  two near-empty screens land above `similarity_threshold` and ONE almost-empty rule becomes a magnet silently
  answering everything. `domain.EmbeddableSalient` is the single definition, enforced three times because
  closing only the query side still lets a long screen match a short stored rule — the incoming situation
  skips the embed, a new short rule is persisted with no vector, and an existing one is stripped by
  `reembed.Reconcile` AND vetoed again in `resolveSignature`'s accept filter (covering the window before a
  rebuild). Reconcile runs at every start and `[embedding]` reload, healing an existing database with no
  migration. Such a rule stays reachable by BM25 and exact hash.
- **Agent-TUI chrome is redacted from pane-tail salients, gated on agent type** — `domain.StripClaudeChrome`,
  `domain.StripCodexComposer`. Chrome is byte-identical across unrelated panes, so it BOTH inflates similarity
  between different screens and eats the `pane_salient_chars` window. It runs BEFORE the window is taken and
  only on the pane-tail branch, and only ever deletes lines it can positively identify — an unrecognized line
  is kept, so two different screens stay different.
  - The `❯` filter is anchored on "last non-empty line" because `❯` is also an option-list caret — **never
    widen it to the bare glyph.**
  - Every filter is ANCHORED at line start: a bare substring test deletes a whole line when the agent merely
    QUOTES the phrase, and the footer window is the entire capture on a short pane.
  - The status bar needs three pieces of evidence together (≥3 pipes, no leading `|`, the terminal-width
    padding run) — the pipe count alone matches a shell pipeline the agent reported running.
  - The banner filter is ARMED only by the `Claude Code` marker at the head, and each line needs ≥2 CORNER
    glyphs (`▐▛▜▌▝▘`, which `█` is not). A capture does not guarantee the logo is on screen, so
    `████████ 80% done` can legitimately be line 1 and stripping it collapses two screens differing only in
    bar length.
  - Accepted trade-offs: a status bar with no trailing token is not recognized (chrome survives — degraded,
    never dangerous), and a pane left with a word or two after the strip trips the over-masking floor.

### The LLM re-ranking judge

**An LLM judge may only ever NARROW what cosine already admitted, and it may never run on the select loop.**
`llm.reranking_command` (off by default) turns `similarity_threshold` into a FILTER: every candidate at or
above it is listed for a one-shot CLI returning `[{"id": n, "score": s}]` by relevance, and hap WALKS it.

- **The judge ranks by RELEVANCE and cannot see a rule's learned STATE, so the head is not always actionable**
  — its best match is routinely one hap may not act on (shadow mode, below threshold, an option no longer
  offered), and only `domain.Decide` knows. `walkRankedDecision` takes the first candidate whose decision is
  not an escalation; when none is, the HEAD escalates, because that is the rule the operator should be asked
  about. This cannot loosen a safety control by construction: every gate not depending on the SIGNATURE is
  fixed in the shared `DecideInput` and vetoes every candidate or none. `llm.reranking_top_k` is the DEPTH of
  that walk, not a prompt cap, which is why `config set` refuses anything below 1.
- **The kill switch is asked for HERE, not inherited** — `startRerank` spawns BEFORE `Decide`'s read, so an
  ungated judge leaves a PAUSED herd launching a subprocess per attention event per parked agent for decisions
  that escalate regardless. Refusing is not a degrade (the caller uses the cosine fallback); a read error
  refuses too.
- **The resume re-reads the pane, and its drop is SILENT** — `rerankSituationHeldStill` discards and logs one
  INFO line. Right direction (the alternative resumes a 30-second-old decision into a live menu) but the branch
  most able to disable the feature unnoticed, so cover it directly rather than through the pipeline tests. It
  carries `handleActionReviewOutcome`'s asymmetry: idle matches on situation TYPE alone, because an idle
  signature hashes a masked head that legitimately differs between the `recent` capture and this `visible`
  re-read; the transition's status falls back to the situation's own, or an empty one mismatches on type and
  drops everything while every test still passes.
- **A vector-search ERROR is not a cosine miss** — `bm25RetryAllowed` refuses a text retry for any STRUCTURED
  salient cosine REFUSED, so collapsing a transient KNN failure into "cosine missed" mints a new key for every
  approval, choice and error screen: the very population this targets.
- **The candidate set is accept-filtered BEFORE the judge sees it** — the same closure the cosine pass uses
  (`min_salient_chars` veto plus `remapAllowed`/`ApprovalRemapCompatible`) gates `matcher.VectorCandidates`.
  Those gates exist because similarity alone bridges two approval screens sharing a verb (#155); delegating
  them to a model puts the wrong answer into a pane.
- **An EMPTY verdict is TERMINAL and skips BM25** — step 4 runs equally when the vector search was clean but
  found nothing above threshold, so a fallen-through veto would be re-admitted by text and the feature becomes
  a no-op that looks like it works. `finishRerank` mints instead (`MatchRerankVeto`).
- **A judge FAILURE is not a veto** — missing binary, timeout, non-zero exit, prose with no array, duplicate or
  out-of-range ids all degrade to `fallback` (computed BEFORE the run, so no error path reconstructs it). The
  veto is an empty array, and equally a verdict scoring entirely below `relevance_score_threshold`;
  `domain.ErrNoRerankVerdict` keeps the two apart, and `lastJSONArray` only accepts a region that already
  unmarshals, so prose brackets are never an answer — except an empty bracket pair, which under last-wins turns
  an earlier answer into a veto, the safe direction.
- **It CANNOT run inline** — the cosine pass returns a `rerankPlan`, `decideAndAct` suspends, and
  `handleRerankOutcome` re-enters `decideAndActResolved`. One flight per agent keyed on `sig.Raw` (there is no
  learning key yet — resolving it is what the run is for), superseded on a different raw and cancelled wherever
  a pending capture is; a token check drops a stale verdict, and `RerankingConfigured` is re-asked so a verdict
  in flight when the operator turned the feature off degrades instead of vetoing. `rerankOutcome` carries
  `fallback` AND `original` because they are not interchangeable: a veto mints from the ORIGINAL, and minting
  from the fallback persists the raw hash while returning the candidate the judge just refused.
- **Only an ESCALATION row carries `match_method`** (`daemon.escalate`, the sole writer, predating this
  feature) — so `MatchRerankVeto` is visible in `hap audit` while `MatchRerank` on a delivered row is not. Do
  not "fix" this by adding provenance to the auto path without deciding what that does for every existing
  cosine/bm25 delivery too.
- **An IN-FLIGHT run is invalidated by the same events the cache is** (`invalidateRerank`). Clearing only the
  cache leaves the hole in its most confusing form: a run started under the old command or threshold finishes
  seconds later, passes the per-agent token check — which is about SUPERSESSION, not staleness — applies its
  answer, and REPOPULATES the cache just emptied; a refresh can also DELETE the very rule the verdict names.
  Each flight carries its `rerankGen` and an older one degrades to the cosine fallback rather than CANCELLING,
  because a reload follows every `hap config set`. **Two bounds make the counter work:** `invalidateRerank`
  bumps UNCONDITIONALLY — a verdict IN TRANSIT is in neither map, so an "is there anything to invalidate" fast
  path lets a pre-refresh verdict commit; and the generation check and the cache write are ONE critical section
  (`commitRerankVerdict`), or an invalidation landing between them caches a pre-invalidation verdict anyway.
  `cancelRerank` keeps its own fast path: it is per-EVENT and keyed on one agent, where a missing entry really
  does mean nothing to cancel.
- **The verdict cache keys on the RENDERED listing, never the candidate signatures** — the listing carries each
  rule's `TopAction`/`Confidence`/`Mode`/`Decisions`, which is what makes the judge say "reuse this", and all
  of those move under an UNCHANGED signature set every time a decision is recorded. Cleared on ANY reload
  unconditionally — never gated on a section compare, or turning the judge off and on again resurrects its old
  answers.
- **A fleet pull retires verdicts only when the listing's INPUTS moved** (`invalidateRerankForKnowledge`). Under
  turso nearly every pull reports a change, and bumping on each one retired almost every in-flight judge run — the
  subprocess ran for nothing. The digest must cover EVERYTHING the listing reads: the candidate rows
  (`SignatureEmbeddingsFingerprint`) AND the learned state (`RuleStateFingerprint`: mode, decision floor, decision
  count + max id). This is not the forbidden "nothing to invalidate" fast path — that asks whether a verdict
  EXISTS, this asks whether the QUESTION changed — and the compare and the bump share one hold of `mu`. Either
  digest unknown invalidates. A change made on THIS node is caught by the next pull's digest, as before.

`match.VectorCandidates` exists for this and re-expresses `MatchVector` rather than duplicating it:
`MatchVector`'s "return the first accepted candidate" is sound only because the list is in descending cosine —
a re-ranker breaks that, so the threshold moves to a caller that sees every candidate. **If any pre-existing
semantic test needs its expectations edited to accommodate this feature, the gating is wrong.**

## Testing practices

- Unit tests are mandatory for behavior changes — table-driven where natural, fakes over mocks
  (`internal/fakeherdr` fakes the herdr socket + CLI; `daemon_test.go` has in-process fakes and `newHarness`).
- **Unix socket paths are length-capped** (~104 bytes on macOS): use `testutil.SocketDir(t)`, never
  `t.TempDir()`.
- macOS temp dirs live under the `/var → /private/var` symlink — compare paths via `filepath.EvalSymlinks`.
- Anything spawning real subprocesses should tolerate a deleted cwd (`llm.Adapter.WorkDir`, `chdirStable`) —
  the daemon can outlive the directory herdr launched it from.
- Where a rule above names a **control** test ("without it the first passes on code that answers one way for
  everything"), that pairing is the point — don't drop one half.

## herdr integration gotchas

The **`herdr`** skill covers CLI usage; these are the hap-specific protocol facts. Version stamps are kept
where the behaviour could revert.

- CLI reads print JSON envelopes (`{"id":…,"result":{…}}`); `pane read --format text` prints plain text.
  `pane get` exposes `cwd` / `foreground_cwd` (a deleted dir renders as `"/path (deleted)"`).
- **`pane read --source recent` is a consuming delta**, not the screen: after one read it can return just the
  cursor line. To recover a standing menu at confirm time, read `--source visible`
  (`herdr.CLI.ReadPaneVisible` / `ports.VisiblePaneReader`).
- **herdr 0.7.5 REMOVED `agent send`** — the old call exits 2 with a usage banner and nothing reaches the
  agent. It quietly did two things and the survivors split them, so `internal/herdr.CLI.submitText` **routes on
  the content**:
  - **single-line → `pane send-text` + `pane send-keys enter`.** Literal terminal input, so a menu digit
    arrives as the KEY it is. Safety-critical: verified live against a real Claude question form,
    `agent prompt "2"` PASTES the 2 as text and its Enter commits whichever option the caret was on — it
    answered "Apple" while hap had chosen "Banana", silently, with a success exit code. **Never route a digit
    through paste.**
  - **multi-line → `agent prompt`.** Writes the text AND its Enter in one request honoring the pane's live
    bracketed-paste mode, so a hand-out lands as ONE message. `pane send-text` is NOT paste-aware — each
    embedded newline is a literal Enter, submitting the first line and typing the rest into the next prompt.

  Both fall back to the legacy `agent send` only on exit status 2 — herdr rejecting the VERB — which keeps
  `min_herdr_version = 0.7.0` honest. A pane-level failure exits 1 with a JSON error body and is returned
  as-is, so a real delivery error is never retried as a second send.
- **`pane send-keys shift+tab` is ACCEPTED and delivers a bare TAB** (herdr 0.7.5) — herdr validates the key
  name, exits 0, and writes `0x09`; `backtab`, `btab` and `S-Tab` are rejected outright, so no key NAME works.
  The chord must be its raw encoding, CSI Z (`domain.ShiftTab` = `"\x1b[Z"`), through `pane send-text` — the
  right transport precisely because it is not paste-aware, so the bytes pass through untouched
  (`CLI.SendChord`, `ports.ChordSender`). This is why `frontend.SetAgentMode` is an open loop re-reading the
  pane after every press: a green exit code is not evidence a chord landed.
- **An agent's permission mode is READABLE ONLY FROM ITS PANE, and only positively** — neither `agent list` nor
  `pane get` carries a mode field, so `domain.AgentModeFromPane` parses the composer footer. **Absence is
  UNKNOWN, never a default**: every Claude mode renders a line (2.1.226, including `⏸ manual mode on`, which
  uniquely omits the cycle hint), so no line means the footer is not shown. **Matching is on the LABEL, never
  the glyph**: `accept edits on` and `auto mode on` both render `⏵⏵`. Codex is the mirror image — it appends a
  right-aligned `Plan mode` segment in Plan and nothing in Default, so "no segment" only means Default once the
  footer itself is recognized. agy follows Codex (`domain.AgyAgentMode`): an `accept-edits · `/`plan · ` prefix on
  the status bar's model segment, none for default — and the bar only counts DIRECTLY under the composer's rule,
  because agy paints the same bar, mode prefix included, under every form and picker.
- **The mode cycle is per-SESSION, not per-agent-type, so a set must detect a closed rotation** — verified live,
  a `--model haiku` Claude session offers three modes while a default-model session in the same build offers
  four, so `domain.AgentModesFor` is a SUPERSET, never a promise. `SetAgentMode` tracks the modes it has
  observed and stops when the rotation returns to one, because pressing to the ceiling parks the agent in an
  arbitrary PERMISSION mode nobody asked for; a failed set also ROTATES THE AGENT BACK (`restoreMode`). The two
  diagnoses are distinct: a mode that did not change means the chord did not land and must keep pressing; only
  a mode that CHANGED into one already seen means the cycle closed.
- **Shift+Tab is REBOUND inside Claude's modals, so a mode press needs positive composer evidence** — a
  standing plan approval renders `shift+tab to approve with this feedback`, so pressing the chord there
  APPROVES THE PLAN. `domain.ClaudeComposerReady` requires the composer SANDWICH (a `───` rule, the `❯` line, a
  second rule), not the bare `❯`, which is also an option list's caret. Refusing because a known form was
  *detected* is not enough; the ordinary composer must be *proven*, and re-proven before EVERY press.
- **A herdr agent name is 1-32 chars of `[a-z0-9_-]` starting with a lowercase letter**
  (`invalid_agent_name`), and `agent start` refuses a name already in use — integration cases derive a unique
  short name from `t.Name()`.
- **`agent prompt` needs the agent to be interactively READY, and says so** — a prompt in the seconds after
  `agent start`, or during claude's release-notes screen, lands in the composer WITHOUT submitting, which is
  what the status-gated retry-Enter loop in `CLI.send` recovers; do not remove it on the grounds that
  submission is atomic now. A pane whose agent is not the foreground process is refused with
  `agent_not_ready`, which is why an externally reported agent can never receive `agent prompt`.
- **Numbered menus want the digit, not the label** — sending the literal label is silently ignored, reading as
  "nothing happened" on confirm. Map with `domain.MenuKeystroke` before delivering.
- **A label that maps to NO option must never be delivered — the literal fall-through commits option 1.**
  Verified live (Claude Code 2.1.220): an unmatched reply at a standing Bash approval runs the command under
  plain "Yes" and reports success, because the agent ignores the letters and the trailing Enter commits
  whatever option the caret rests on. So "no digit could be mapped" is not a safe default:
  `domain.UnmatchedMenuReply` is the gate and **all FOUR send paths** refuse on it — `daemon.act`, the LLM
  promotion in `handleLLMOutcome`, the rewritten reply in `handleActionReviewOutcome`, and `deliver.Deliver`.
  - Two things make a correct label fail to map, both load-bearing. **Typography** — the same build renders
    `don’t` with U+2019 while every rule, LLM answer and fixture here writes ASCII, so comparisons go through
    `domain.FoldMenuText`. **Drift** — a rule learned on one render names an option no longer offered, which is
    exactly what must escalate.
  - Three ordering rules are deliberate and easy to undo: the gate runs AFTER the multi-tab answer-series and
    remote-environment branches (each answers its own protocol); AFTER `llm.enable_rewrite_action` dispatches
    in `act`, because adapting a drifted label is what the rewrite is for (and the result is re-checked, so
    nothing skips the gate that way); and matching is unique-or-refuse on BOTH the exact and prefix passes,
    since one capture can hold two renders numbering the same label differently.
  - Two accepted trade-offs: an approval whose real prompt is a bare `y/n` with unrelated numbered lines in the
    scrollback now escalates instead of typing `y`; and an UNREADABLE pane refuses only when the decision's own
    capture proves a menu was standing (`req.PaneExcerpt`), so legacy rows with no excerpt behave as before.
- **A digit does NOT always commit — AskUserQuestion has two protocols, per tab.** On **plain** options the
  digit selects AND auto-advances; on **preview** options (list left, `┌──┐` box right) the digit only **moves
  the caret** and **Enter** commits. The footer is identical in both and never mentions digits, and one form
  mixes them (a preview form's generated Submit tab renders plain), so blind digit-only delivery is a silent
  no-op. **Never plan a whole keystroke series up front** — `internal/mcqdeliver` presses, re-reads, and only
  presses Enter if the answer did not commit (refusing if the caret never reached the chosen option).
- **Claude's "Select remote environment" picker reports IDLE, not blocked** — herdr shows no blocked status
  while the modal stands, so hap detects it structurally (`domain.ClaudeRemoteEnvForm`) and classifies it as a
  parked APPROVAL at idle/done, the same exception pattern as Codex's Plan approval. Despite its "Enter to
  select" footer the digit alone COMMITS, but all paths still answer adaptively via `mcqdeliver.ClaudeRemoteEnv`
  in case a build ships the caret binding, failing closed when the learned label matches no offered environment.
- One `events.subscribe` per socket connection; status subscriptions require a concrete `pane_id`; existing
  panes are replayed as `pane_created`.
- **A status subscription costs herdr CPU per pane, so only AGENT panes are watched** (`Subscriber.agentPanes`,
  herdr 0.8.2): with 13 live panes (2 agents) the per-pane subscriptions were ~5.5 points of the server's CPU —
  more than half its load with the daemon up — against ~2.8 for the agent panes alone, while the discovery stream
  and hap's CLI calls were negligible. The label comes from `pane.list` (authoritative; an agent that exited
  clears it) or `pane.agent_detected`, and a label newly attached to an existing pane resubscribes
  (`upsertPane`) — too late for ONE event by construction: herdr emits the detection and the agent's first status
  in the same update, so a resubscribe that ADDS an agent pane replays its status from the `pane.list` snapshot
  (`replayStatus`; not on the first subscribe, which the startup reconcile covers). **A listing that labels NO pane falls back to watching every pane** (and keeps discovery's
  labels) — `min_herdr_version` is 0.7.0 and a herdr not reporting labels in `pane.list` would otherwise leave
  every agent running at daemon start unwatched; a herd with no agents takes the same path, harmlessly.
  **Test trap:** `fakeherdr.AddPane` is a plain SHELL — a test pushing status must use
  `AddAgentPane` (or `PushAgentDetected` first), exactly as real herdr only reports status for a detected agent.
- Adding a pane makes the subscriber reconnect ("pane set changed", 1s backoff) — tests pushing transitions
  right after `AddPane` must wait past the resubscribe.
- The herdr binary resolves via `HERDR_BIN_PATH` (fallback: `herdr` on PATH); the events socket via
  `HERDR_SOCKET_PATH`.

## Where things live

| Path | What |
|---|---|
| `cmd/hap` | entrypoint: daemon / TUI / CLI / `mcp` subcommands |
| `internal/domain` | pure decision core, signatures, safety heuristics |
| `internal/daemon` | monitor loop: subscribe → classify → decide → act/escalate |
| `internal/classify` | pane-content classifier + golden fixtures |
| `internal/mcqdeliver` | answers a live multi-tab MCQ form, verifying each keystroke landed |
| `internal/domain/agentmode.go` | parses an agent's permission mode out of its composer footer; proves the composer is safe to press into |
| `internal/llm` | operator LLM CLI adapter (argv template, auto-repair) |
| `internal/mcpserver` | stdio MCP server (`get_context`, `submit_decision`) |
| `internal/herdr` | herdr CLI + events-socket adapters |
| `internal/store` | SQLite persistence (WAL; `context_json` is an opaque blob) |
| `internal/taskfile` | advisory file lock behind every checklist read-modify-write |
| `internal/tasklocator` | the ONE canonicalizer for a task-list locator + provider resolution (pure) |
| `internal/taskstore` | task-list backends: `local` (default), `gist` (opt-in, the only GitHub SDK importer) and `dbtask` (the `sqlite` provider: lists as `task_lists` rows, `db://<node>/<name>`, synced under turso) |
| `internal/selfpath` | resolves a live `hap` binary (an upgrade unlinks the running one) |
| `internal/tuisession` | flock registry of live `hap tui` processes; closes the oldest past `[tui] max_instances` |
| `internal/streamlog` | machine-local SQLite event log behind `hap stream orchestrator` (its own file, NOT a store table) |
| `internal/profiling` | opt-in rolling CPU/heap profiles (`HAP_PROFILE_DIR`); files only, no listener |
| `internal/updatecheck` | GitHub release check — one of exactly TWO `net/http` importers (NFR-007 allowlist) |
| `internal/fakeherdr`, `e2e_harness/` | test fakes and the e2e driver |
| `docs/architect/herd-auto-prompter-architecture.md` | consolidated architecture doc (FR-xxx / NFR-xxx ids used in comments) |
