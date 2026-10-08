# CLAUDE.md

Herd Auto Prompter (**hap**): a Go plugin for the herdr terminal multiplexer. It watches every agent pane,
auto-answers when a learned rule is confident, and escalates to the operator (or a local LLM CLI) otherwise.

**How to use this file.** Each rule names the identifier that implements it and the direction it must fail.
The rationale (mechanism, measurements, incident) lives in that identifier's doc comment — read it before
changing anything a rule covers. This file only adds what the code cannot tell you: couplings across files,
**test traps** (why a regression shipped green), and facts verified against external tools. Find guarding
tests with `grep -rn "func Test<Topic>" --include=*_test.go`.

Skills (`.claude/skills/`): `herdr` (drive herdr), `hap` (operate via CLI), `hap-development-local` (link,
rebuild, hot-swap the daemon with `hap daemon --ensure`, live-test). `hap --skill` prints the hap skill.

## Build, test, lint

```sh
bash scripts/check-submodule-gitlink.sh        # submodule must be a gitlink, not a symlink (#265)
bash scripts/setup-native.sh                   # one-time native deps (llama.cpp, FAISS)
go build -tags "vectors cpu" ./...             # both tags always, or it fails to link
go test -tags "vectors cpu" ./... -count=1     # what CI runs; run before every Go commit
gofmt -l . | grep -v submodule && go vet -tags "vectors cpu" ./...
golangci-lint run --build-tags "vectors,cpu"
```

- Real-model embedder test needs `models/all-minilm-l6-v2-q8_0.gguf` (or `HAP_TEST_EMBED_MODEL`).
- Golden fixtures: `UPDATE_GOLDEN=1 go test ./internal/classify/`, then review the diff.
- **Loaded machine: export `HAP_TEST_TIMEOUT_SCALE` (CI uses 4) or cap `-p`** — `testutil.Scale` counts
  cores, not load; that is #431's "different test fails each run".
- Profiling: `HAP_PROFILE_DIR=<dir> [HAP_PROFILE_SECONDS=60] hap daemon --restart` (`internal/profiling`).
- Pipeline smoke: `go build -o /tmp/e2e ./e2e_harness && /tmp/e2e <short-dir> <hap-bin> <config-dir> <state-dir>`.

### Integration suite (real herdr + agents)

`test/integration/`, build tag `integration`; cases skip when a dependency is absent. **Run once after any
feature, before the PR** — the unit suite fakes herdr.

```sh
go test -tags integration ./test/integration/ -v                                   # inside herdr, or HERDR_BIN_PATH
HAP_ITEST_CLAUDE=1 go test -tags integration ./test/integration/ -v -timeout 20m   # also HAP_ITEST_CODEX / HAP_ITEST_AGY
go test -tags "integration vectors cpu" ./test/integration/ -v                     # + real-model semantic case
```

Traps:
- A test asserting a CONFIRM runs its own daemon via `testDaemon.App`; an `App` without `DaemonInfo` reads as
  "no daemon". Never point one at the operator's live store (under turso it pushes to their cloud DB).
- A scratch pane the daemon classifies must SCROLL (`fillViewportSh`): `--source recent` is empty for a pane
  that fits on screen.
- Wait for content (`waitForPaneText`), never sleep.
- The operator's own daemon watches scratch panes too: agy cases `quietOperatorDaemon`; a front end needs a
  published roster (`publishLiveRoster`).
- `TestRealShiftTabKeyNameIsStillBroken` is a version-gated tripwire. `TestRealClaudeConsult` needs a path
  outside claude's auto-approved dirs. Mode-cycle cases are the only check that the Shift+Tab encoding and
  the mode-indicator labels still work.

## Commits, branches, changelog, release

- Format `#<issue> <type>: <subject>` (Conventional Commit; `#0` if no issue). Types `feat fix docs test
  refactor chore`; breaking `feat!:`. Hooks must run — no `--no-verify`.
- Never commit to `main`. Non-trivial work: `git-worktree` skill (`worktree-agent-noN` beside the repo);
  remove it and delete the branch (local + origin) after merge. If others may be working in the tree, stage
  only your own changes and move to a fresh worktree.
- **Changelog is MANDATORY, as a fragment, never an edit to `CHANGELOG.md`, never a version number:**
  ```sh
  cat > changelog.d/$(git branch --show-current | tr / -).md <<'EOF'
  - Fixed the thing that used to happen
  EOF
  ```
  Flat verb-first one-liners, what it means for the reader; mark **Breaking.** Auto-release assembles them.
  For minor/major, run `bash scripts/assemble-changelog.sh X.Y.0` in the same PR (release refuses to tag
  otherwise). (`CONTRIBUTING.md`'s changelog paragraph is stale; this section wins.)
- **Release** (`.github/workflows/auto-release.yml`): `version` in `herdr-plugin.toml` TRAILS releases.
  Patch: just merge — never bump by hand. Minor/major: set `version` inside the feature PR.
  - Never put `[skip ci]`-family keywords in a squash-merge message that should release (suppresses the
    tag build). Keep `[skip release]` out of ordinary merges.
  - Release build failed after tagging → re-run `release.yml`, never auto-release.
  - Tagged manifest version must equal the tag; verify with `gh release view vX.Y.Z` (3 binaries, 3
    tarballs, model, SHA256SUMS). Never edit `internal/buildinfo.Version`. Bump `min_herdr_version` only
    when adopting new herdr APIs.

## Architecture rules

### Boundaries
- `internal/domain` stays pure (`TestDomainPurity`); side effects behind `internal/ports`.
- Optional capabilities are optional port interfaces, type-asserted at the call site — don't grow `HerdrPort`.
  **Test trap:** the daemon suite's `failingStore` embeds `ports.StorePort`, so a capability not forwarded
  there is silently off suite-wide.
- Daemon path never panics; errors → escalate + audit + log, under `logging.Guard`.
- Safety controls (kill switch, never-auto, rate guard, retry ceiling) re-gate LLM and learned answers
  alike. New destructive shapes → `internal/domain/testdata/irreversible_corpus.txt`.
- Nothing that shells out repeatedly runs on the select loop (pattern: `consultLLM` / `llmResults`).
- Egress: only `internal/updatecheck`, the `github_gist` backend, the `turso` engine and the `libsql` engine
  (HTTP only in `internal/store/libsql/hrana.go`), all opt-in. `internal/privacy` enforces by import path.
  The gist adapter must keep `github.WithURLs` / `github.WithTimeout`.

### Config surface
- Every config.toml writer is a `hap config` subcommand; every key is reachable from the CLI (scalars in
  `frontend.ConfigFields`, list/map sections via `configListCommands`; both registry tests also fail on
  stale entries). Moved spellings print their note on **stderr** (stdout is parsed).
- List editors: every `[[task_sources]]` field has a `set` key; removal compares the WHOLE entry; inserts
  respect `config.CaptureDelay`'s first-match order; a new task source is always APPENDED (its index is a
  public selector) — in both `AddTaskSource` and `addTaskSourceIfAbsent`. `hap config env` never prints values.

### Front end decides, daemon does
Anything reaching a live pane or a node-local identifier is queued to `agent_actions` for the owning node
(`daemon.executeAgentAction`). Generated-task confirms are queued unconditionally
(`queueGeneratedTaskConfirm`); pane access is received via `ports.TaskSendHost`. Keep `ConfirmGeneratedTask`
/ `AcceptGeneratedTask` separate (`automated` skips `ResolveEscalation` + `InsertCorrection`);
`CorrectionID` stays 0 on queued rows; `side_effect` is marked inside `Send`. `refuseIfAgentBusy` must keep
`domain.SuggestionStaleMarker` (the TUI's offer keys on it). Orchestrator-authored actions
(`domain.OrchestratorAuthor`) are screened (`daemon.actionScreen`) and refused while paused.
**Test trap:** `internal/daemon` cannot import `internal/frontend`, so daemon tests drive a FAKE seam; the
real confirm is proven only in `internal/frontend`, and TUI/CLI suites must drain through the real confirm.

### Multi-tab forms and auto-accept (`internal/daemon/autoaccept.go`)
- A swept form's baseline is an AGGREGATE (`AggregateMCQFrames`); never compare it to one frame — compare
  frame-wise (`mcqFormHeldStill`). Its four gates refuse as `heldStillUnevaluable`, never `heldStillNo`.
  **Test trap:** `internal/domain/testdata/mcq_preview_*.txt` is the only preview-layout fixture.
- An aggregate's HEAD is load-bearing: its own budget (`aggregateMaxRunes`, `excerptBudget`); the gate is a
  strict parse. **Test trap:** `seedAgedSweptEscalation` used to skip truncation.
- Option labels: `trimPreviewColumn`, `wrappedOptionLabel`.
- Checkbox tabs: `checked ⊆ chosen` enforced at DELIVERY in three places (`domain.CheckedOutside` in
  `reverifyMultiSelect`, `frontend.verifyTabBaseline`, `mcqdeliver.toggleTab`), never at capture.
- `claimBlockedBy` is the complete last look before a claim. Content-safety refusals are `errOutboundRefused`
  (not a delivery fault, no retry budget). Failed reverts retry every tick (`retryAutoAcceptRevert`).
  `notePending` logs each refusal once per (row, reason), placed after `stillEligible` is set.

### Full self-prompting (FSP)
- FSP may widen the YES side of Guard 3, never the NO side: `mcqSalientHeldStill`, `unstructuredHeldStill`
  decline to `heldStillUnevaluable`. The option set alone is never sufficient identity;
  `domain.LiveMCQMatchesSalient` must mask both sides.
- `@noop` under FSP is retired (`ReasonAutoDismissNoop`), still honouring kill switch / disable / pause.
- `honour_limits = false` makes `[limits]` inert as one CLAUSE per gate (`domain.RateLimits.Inert`;
  `limitsInert` / `limitsInertFor`), never an early return — `sweepAllowed`, `autoAcceptAgentSuppressed`,
  `eligibleIdleAgents` share the disable/kill checks. Test these guards directly.
- A busy pane is `domain.ReasonPaneBusy`, never `ReasonRateLimited`. `fspCeilingReached` ignores `Paused` and
  only reads deliverable rows; `continue`, not `break`, after a stand-down. The stand-down latches in memory
  first and writes config off the loop. `autoAcceptNeedsFinalize` is a map id→was-FSP.
- Generated tasks are screened on the RENDERED prompt, and again inside the seam via its `screen` callback.
  **Test trap:** only a `frontend` test proves the real seam calls it.
- Every status check on the generated-task path must allow the daemon-owned row (the early
  `audit.Status != "escalated"` check included). **Test trap:** shipped green on a fake seam.
- Every generated-task hand-out gets a ledger row (`recordTaskReservation`), operator's included.
- Orchestrator (`daemon/orchestrator.go`): filtered at EVERY ingest point (`isOrchestrator` /
  `withoutOrchestrator`); identity is `<state>/orchestrator.json`; created only fail-closed
  (`domain.OrchestratorLaunch`, `orchestratorPermitted`).
- A generated task may not name another agent (`domain.StripForeignAgentGeneratedLines`, node-local
  `AgentNames`, all-dropped branch above `declined`).

### Queue notices, snooze, wait, operator presence
- Only queue notices (`domain.LatchedPerParkedEpisode`) may be withheld, through `daemon.queueNoticeWithheld`.
  The cooldown and background-work evidence apply to `no_task_source` ALONE (`noticeCooldownApplies`); the
  cooldown is not pruned in `noteIdleAgents`' new-episode block. **Test trap:** drive the re-park, not just
  the working flip.
- `domain.BackgroundWorkRunning` may only WITHHOLD; read from memory (`backgroundWorkFor`), never a
  per-row shell-out. Don't move it into `internal/classify`.
- `hap wait` (`domain.AgentWait`) is lifted by the CLOCK, not by working; expiry is read, never written;
  payload carries a duration. `hap snooze` is separate from `disabled`; it gates only `queueNoticeWithheld`,
  `eligibleIdleAgents` and the `ActionGenerateTask` arm, and must not touch
  `WithAgentAutomation`, `deliverreply.go`, `autoaccept.go`, or `generatedtask.go`'s barrier; it clears via
  `ClearAgentSnoozeIfSet`.
- `domain.OperatorTyping`: `known == false` is common and withholds nothing. Asked in `generateTask` ABOVE
  `HasPendingLLMConsult` (no stranded `llm_requests` row); auto-accept asks in
  `claimBlockedBy`, not `autoAcceptDeliver`. codex answers unknown on purpose.
- Orchestrator replies get the prose screen (`deliverReplyScreen`). Refused queued actions escalate
  (`escalateRefusedAction`) after resolving `a.Target`.

### Task sources and hand-outs
- `enable_auto_send_task_when_idle` skips learning gates, never safety ones (`domain.Decide`).
- A hand-out is confirmed only by a `working` transition (`task_reservations`, `reclaimStrandedTasks`); one
  unconfirmed hand-out per agent (`agentsAwaitingHandout`). `staleHandoutTTL` is asked before
  `BackgroundWorkRunning`. **Test trap:** only the reservation ID and the second send discriminate.
- A pending escalation never benches an agent from the idle poll; don't key on the audit `Trigger`.
- An empty per-source `provider` is the inheritance and must never be materialized (`normalizeTaskSources`
  runs on Load AND Save). The top-level provider IS pinned in `Load` before the decode.
  **Test trap:** file-backed fixtures declare it (`localFSApp` / `localFSCfg`); `testApp` stays on the default.
- A locator is never a file outside `internal/taskstore/local`: mutate via the store
  (`frontend.mutateList` / `mutateTask`, `daemon.mutateTaskList`). Compare locators (`isBootstrapList`),
  canonicalized only by `tasklocator.Canonical`. Guard: `TestOnlyTheLocalBackendTouchesATaskListAsAFile`;
  remote-path tests belong in `remote_confirm_test.go`.
- A task list is never created blank (`newListHeader`, `ports.EnsureCreator`, `gist.Store.put` →
  `ErrBlankContent`): GitHub answers 422 `missing_field` on `files`. **Test trap:** `fakeGist` must keep
  emulating that 422.

### Store, nodes, sync
- Node-owned rows carry `node_id`; every operational statement filters `node_id = self`
  (`TestEveryNodeOwnedStatementIsNodeScoped`, by AST — SQL must be a literal at the call site, not a const or
  struct field). Store suite runs under `HAP_STORE_TEST_MODE=sqlite|proxy|turso|libsql|libsql_replica`.
  Turso two-node tests need `tursodb` on PATH; `HAP_TURSO_TEST_URL`/`_TOKEN` and `HAP_LIBSQL_TEST_URL`/`_TOKEN`
  target real servers.
- `signatures`, `signature_embeddings`, `signature_snapshots`, `decisions` are fleet-wide — **never add a
  `node_id`**. `embedder.ModelIDFor` is the fleet identity; anything comparing a row's model must use it.
- `store.importer.copyAll` is the ONE copier between engines. `migrateNodeScoped` / `migrateExplicitID` mirror
  `nodescope_test.go` (`TestMigrateScopeListsMatchTheGuard`); every table is copied or listed in
  `migrateNotCopied`. Source node selects, destination node stamps.
- libsql replica (`internal/store/libsqlreplica`): never a TEMP table in server SQL (sqld refuses it — add
  such shapes to `hranafake.sqldRefuses`; run `TestLiveSyncStatementShapes` after changing sync SQL); a
  replay never uses INSERT OR REPLACE; tables prefixed `hap_` stay local; push-wins columns are covered by
  `TestPushWinsCoversEveryEscalationTransition`. Server errors keep `domain.LibSQLServerErrorPrefix`.
- Schema DDL on a shared DB only under the lease (`turso.PrepareSharedSchema`); a definitive loss latches.
  **Test trap:** `schema_lease_test.go` skips without `tursodb` (and no CI job installs it); use the fake
  `SchemaSyncer` (`schema_lease_latch_test.go`).
- `database.sync_paused` is read LIVE and gates server round trips only — shutdown push included, local
  checkpoint not; `checkFleetSyncWedged` refuses while paused; unpausing pushes.
- A periodic write must be CONDITIONAL (each write arms the 2s turso push): `rosterRowUnchanged` MIRRORS
  `upsertRosterRow` field for field; read-only transactions report no write (`txWrote`). `domain.NodeHeartbeat`
  is bounded above by `daemon.actionStaleAfter`.
- A wedged sync restarts with `--restart`, never `--ensure` (`checkFleetSyncWedged`), only for process-local
  faults (`domain.SyncFailureProcessLocal`).
- Retention (`PruneAgedRows`): `audit_log` and `decisions` are never swept; roster rows only with a tombstone.
  **Test trap:** `kill_events` survivors are per SCOPE — seed non-global rows.
- The ONE cross-node delete is `store.PruneOfflineNode` (`hap nodes prune`): deliberate, operator-run, freshness
  re-checked inside its transaction, disabled agents' names kept (a returning node must not get them back enabled).
- Front ends poll a change token (`Store.Revision`, `frontend.App.ChangeKey`), one poll in flight. Config
  never enters the database; front ends draw ids from the daemon with no local fallback.

### agy
herdr never reports an agy modal as blocked; forms are parsed structurally (`internal/domain/agy.go`,
`docs/designer/agy-support.md`).
- A reply to an agy form is KEYS (agy commits on the digit alone; a trailing Enter answers the next screen).
  Every send path must ask `domain.AgyFormSituation` and route to `mcqdeliver.Agy` — today `daemon.act`
  (ahead of the action-review rewrite), the LLM promotion in `handleLLMOutcome`, and `deliver.Deliver`.
  Every verified answer must `recaptureAfterAgyAnswer`, or the form stalls after question 1.
- Anything else typed into agy needs `domain.AgyComposerReady` — asked by `deliverAutonomousClaimed`, the LLM
  promotion, `deliver.Deliver`, `refuseIfAgentBusy`, `requireIdleForHandout`, `actionTaskSendHost.Send`.
  A new hand-out path must ask it too.
  **Test trap:** `fakeHerdr` has no `SendToAgent`, so the proof lives at call sites.
- Never set `MCQKind`/`AnswerCount` on an agy situation.

### Claude session-name sync (`[agents] sync_claude_session_name`)
Read only from a proven composer; absence is UNKNOWN. Quiescence is asked in `applyClaudeSession` and again
live in `pushSessionRename` — not a third time. `NormalizeAgentName` and `SuffixedAgentName` must stay fixed
points. **Test traps:** push cases must satisfy `parkedAndSettled` (pin the listing AND backdate
`d.idleSince`); flip tests expose the composer only through `--source visible`.

### Semantic matching and the re-ranking judge
- Matching degrades, never blocks: vector → BM25 → exact hash. `signature_embeddings` is the source of truth;
  the bleve index is a disk-backed cache.
- Short pane-tail salients are never embedded on either side (`domain.EmbeddableSalient`); structured
  salients are exempt. If existing semantic tests need a lowered floor, the scope is wrong.
- Chrome stripping (`domain.StripClaudeChrome`, `domain.StripCodexComposer`) only deletes positively
  identified, line-anchored lines; never widen the `❯` filter to the bare glyph.
- The judge (`llm.reranking_command`) may only NARROW cosine's set and never runs on the select loop
  (`rerankPlan` → `handleRerankOutcome`). `startRerank` asks the kill switch itself (it runs before
  `Decide`). Candidates are accept-filtered before the judge sees them (`remapAllowed` /
  `ApprovalRemapCompatible`). An empty verdict is terminal (`MatchRerankVeto`); a failure is
  not a veto. Invalidation bumps unconditionally (`invalidateRerank`, `commitRerankVerdict`). If existing
  semantic tests need edited expectations, the gating is wrong.

## Testing practices
- Unit tests mandatory for behavior changes; fakes over mocks (`internal/fakeherdr`, `newHarness`).
- Socket paths: `testutil.SocketDir(t)`, never `t.TempDir()`. macOS temp paths: `filepath.EvalSymlinks`.
- Real-subprocess code must tolerate a deleted cwd (`llm.Adapter.WorkDir`, `chdirStable`).
- Keep both halves of control-test pairs.
- **fakeherdr traps:** `AddPane` is a plain shell — use `AddAgentPane` to push status; `events_lost` cases
  need `SetSubscribeReplay(false)`; adding a pane triggers a ~1s resubscribe. The fake implements no
  claude-typed-input setter (`TestReloadPushesClaudeTypedInputToTheHerdrAdapter` is the only proof).

## External facts (verified live; the code cannot prove these)
- `pane read --source recent` is a consuming delta; use `--source visible` to re-read a screen.
- herdr ≥0.7.5 has no `agent send`. `CLI.submitText` routes single-line → `pane send-text` + Enter,
  multi-line → `agent prompt`; legacy `agent send` is a fallback ONLY on exit 2 (exit 1 is returned, never
  retried — a retry is a double send). **Never route a menu digit through paste** — it commits the caret's option.
- A paste into claude reaches its model as untrusted `<pasted_content>`; `[agents] claude_typed_input`
  types instead (`internal/herdr/typed.go`). Pacing, not burst size, is what avoids the paste heuristic.
- A label matching no option commits option 1 — `domain.UnmatchedMenuReply` gates all four send paths
  (`daemon.act`, `handleLLMOutcome`, `handleActionReviewOutcome`, `deliver.Deliver`).
  Send digits (`domain.MenuKeystroke`), not labels.
- AskUserQuestion: preview tabs need digit + Enter, plain tabs commit on the digit — `internal/mcqdeliver`
  presses and re-reads, never plans a series.
- Before herdr 0.8.2 `send-keys shift+tab` sends TAB; hap sends CSI Z (`domain.ShiftTab`) until
  `min_herdr_version` ≥ 0.8.2. Shift+Tab inside a claude modal approves the plan — require
  `domain.ClaudeComposerReady`.
- Permission mode is readable only from the pane, only positively (`domain.AgentModeFromPane`); the mode
  cycle is per SESSION, so `SetAgentMode` detects a closed rotation.
- Claude's remote-environment picker reports idle (`domain.ClaudeRemoteEnvForm`).
- Concurrent tool approvals (parallel subagents) are PAGED in one dialog ("1 of 3"); answering a page draws
  the next in place with herdr still `blocked` — no event. Only the post-action self-check sees it
  (`followUpPromptStanding`, gated on `domain.ClaudeModalAdvanced`); every unattended send must arm it.
- Claude commits a permission dialog on the DIGIT alone, so an Enter after it approves the next queued page
  unseen (#564). A mapped Claude menu digit is a KEY (`mcqdeliver.ClaudeMenu`): Enter only when every re-read
  shows `domain.ClaudeMenu.SameStanding` (counter KEPT — never `ClaudeModalAdvanced`) AND the digit MOVED the
  caret onto itself (a digit already under the caret gets no Enter: a late redraw looks identical).
  Every send path asks `domain.ClaudeMenuDigit` — `deliver.Deliver`, `deliverAutonomousClaimed`
  (`delivery.menuDigit`), the LLM promotion; the daemon claims the pane BEFORE the digit, settles off the
  loop, then arms the self-check. Past the digit nothing is a delivery failure (auto-accept would retry).
  **Test trap:** the daemon fake answers digits through `onKey`; `menuDigits` fails on a text send.
- One request per herdr socket connection. herdr ≥0.9.0 replays no existing panes on subscribe; `events_lost`
  means resubscribe + resync. Only agent panes get status subscriptions.
- Agent names: `[a-z][a-z0-9_-]{0,31}`, unique. `agent prompt` right after `agent start` may land without
  submitting — don't remove the status-gated retry-Enter loop in `CLI.send`.
- `HERDR_BIN_PATH` / `HERDR_SOCKET_PATH` override the binary and events socket.

Full architecture: `docs/architect/herd-auto-prompter-architecture.md` (FR-/NFR- ids used in comments).
