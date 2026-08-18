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

The hap skill also ships inside the binary: `hap --skill` prints it, and
`hap skill install <claude|codex|agents>...` (or the TUI Config-tab shortcut)
installs it into `~/.claude/skills/hap/`, `~/.codex/skills/hap/`, or
`~/.agents/skills/hap/`.

## Build, test, lint

The semantic matcher links native code (llama.cpp via CGO, FAISS behind bleve's
`vectors` tag), so **the native deps are needed once** and the `vectors cpu` tags
always — a build without both fails to link.

```sh
bash scripts/check-submodule-gitlink.sh        # seconds: submodule must be a gitlink, not a symlink (#265)
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
HAP_ITEST_CLAUDE=1 go test -tags integration ./test/integration/ -v -timeout 20m # also drive a real claude (spends tokens; several real-claude cases can exceed the 10m default)
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
  the rendering where a digit only moves the caret, which blind digit delivery no-oped on;
  `TestRealClaudeOneQuestionMultiSelectMCQDelivery` (needs `HAP_ITEST_CLAUDE=1`) drives a real
  ONE-question multi-select form — the shape whose footer carries no tab hint, so it read as a
  plain menu and got a bare digit that only toggled its checkbox — and asserts the answer
  toggles, advances to Submit, and commits. It FAILS (does not skip) when a form is on screen
  but `MultiTabForm` misses it, so the detection regression can never pass silently.
  `TestRealClaudeModeCycle` / `TestRealCodexModeToggle` (`agentmode_test.go`, needs
  `HAP_ITEST_CLAUDE=1` / `HAP_ITEST_CODEX=1`) DISCOVER the session's cycle (never assume
  `AgentModesFor` — see the per-session rule above), drive the agent to every mode it
  offers, and assert a mode it does NOT offer fails cleanly and leaves the agent where it
  was. They are the only check that the Shift+Tab chord ENCODING still reaches an agent and
  that the mode INDICATOR still renders the labels the parser matches, both of which live
  in the agent's build, not herdr's. `TestRealClaudeModeRefusesAStandingModal` proves the
  safety gate against a real modal — it raises `/model` (deterministic, no tool call
  needed) and asserts the picker is still standing untouched afterwards.
  `TestRealShiftTabKeyNameIsStillBroken` is a TRIPWIRE on the workaround: it FAILS if
  herdr's `shift+tab` key name ever starts working, which is the signal to delete
  `domain.ShiftTab`/`CLI.SendChord` in favor of a plain `pane send-keys`.
  `TestRealHerdrNotification*` / `TestRealInHerdrDetection` (`notification_test.go`) drive the
  socket notifier the TUI alerts through: the `notification.show` result shape (`shown` must
  agree with a `reason` the TUI knows), that two calls in a row both land (herdr closes the
  connection after each answer, so a pooled connection would EPIPE), and that an empty
  normalized title is refused locally AND still refused by herdr. They raise real toasts.

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

## Changelog (MANDATORY)

**Every change gets an entry — including patch releases.** No exceptions for
"small", "internal", or "just a fix": if it merges, it is in the changelog. A PR
without one is incomplete. Entries are written as **fragments in
`changelog.d/`**, never into `CHANGELOG.md` directly.

- **Never edit `CHANGELOG.md` by hand, and never write a version number.** Add a
  fragment instead — a new file nobody else's PR can touch:

  ```sh
  cat > changelog.d/$(git branch --show-current | tr / -).md <<'EOF'
  - Fixed the thing that used to happen
  EOF
  ```

  Just the bullets, no heading. `changelog.d/README.md` has the full format.
- **Why:** every PR used to insert its section at the top of one shared file, so
  two open PRs always conflicted on the same lines even when the changes were
  unrelated — and each had to GUESS its version, which is only knowable when the
  release is cut. Fragments make the conflict structurally impossible and delete
  the guess.
- On merge, the auto-release workflow runs `scripts/assemble-changelog.sh
  <version>`, folding every fragment into `CHANGELOG.md` under the real version
  and deleting them — inside the same commit that bumps `herdr-plugin.toml`.
  A CI job fails any PR that changes releasing code without a fragment.
- **Minor/major is the manual exception**: you hand-write the version into
  `herdr-plugin.toml` inside your PR (see *Version bump & release*), and no bump
  commit is ever created to assemble into — so run
  `bash scripts/assemble-changelog.sh X.Y.0` in that same PR and commit the
  result. The release refuses to tag while unassembled fragments remain.
- Style: a flat list of verb-first one-liners — `Added …`, `Fixed …`,
  `Changed …`, `Removed …`. No sub-sections.
- Write what it MEANS for the reader, not what the diff did. GitHub already
  generates a per-release list of PR titles; this file exists for what a title
  cannot carry — the bounds of a new action, what a changed default now does,
  what a fix stops happening.
- Mark a breaking change **Breaking.** at the start of its line, matching the
  `feat!:` commit type.

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
- Between the bump merge and the release publishing (~15 min), main's manifest names a
  version with no assets — install.sh's ~60 s curl retry only bridges the post-publish
  upload gap, not the build. Installs no longer 404 there: install.sh **falls back to the
  newest earlier release that has assets** (git tags, newest-first, HEAD-probed), warns
  loudly which version it actually installed, and leaves the operator to `hap update` once
  the intended one publishes. The fallback is refused for an explicit `HAP_VERSION` pin,
  under `HAP_NO_FALLBACK` (any NON-EMPTY value — set-but-empty means unset, matching
  `HAP_VERSION=`), and on a checksum mismatch — corruption is never
  answered with a downgrade. `--ref vX.Y.Z` is NOT one of those refusals: it pins the git
  clone, which install.sh cannot see, so it only avoids the fallback by naming a release
  that already has assets. Detached HEAD is not a usable substitute signal either, because
  auto-release tags the bump commit on main.
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
- **Everything that writes config.toml is a `hap config` subcommand** — one place to learn,
  and the reason `hap config` is the ONLY visible command in the help's Configure group
  (`TestConfigureGroupHasOneVisibleCommand` fails on a second one). Topics are TWO-word
  registry entries (`config rules`, `config task-source`, `config classifier`,
  `config capture-delay`), resolved longest-spelling-first in `cli.Run`, so each is
  dispatched and documented on its own rather than routed by the parent's handler — that
  is what makes `hap config rules --help` reach the topic's page. `hap task` is
  deliberately NOT here: it edits checklist ITEMS in an agent's markdown file, which is not
  configuration. The four were top-level verbs once; their old spellings still resolve
  (`Command.MovedFrom`) and print a migration note on **stderr**, never stdout, because
  these verbs print tab-separated listings that scripts parse.
- **Every config key is reachable from the CLI** — the TUI is a convenience, not a
  capability, and config.toml is never something an operator must open by hand. SCALAR keys
  go in the `frontend.ConfigFields` registry (`hap config set`); `TestEveryConfigKeyIsRegistered`
  walks `config.Config` the way BurntSushi's decoder does and fails on any unregistered one.
  ARRAY and MAP sections cannot be a `config set` key — a list element is addressed by
  POSITION and a map entry by NAME — so each gets a verb (`hap config rules`, `hap config task-source`,
  `hap config classifier`, `hap config capture-delay`, `hap config env`) and is named in
  `configListCommands`, which `TestEveryConfigListHasACLICommand` holds to the same
  by-construction standard. Both tests also fail on a STALE entry, so neither map can claim
  coverage for a key that no longer exists. Three rules bind the list editors: **a list
  ELEMENT's own fields are held to the same standard** where the element is long-lived —
  every `[[task_sources]]` field has a `hap config task-source set` key
  (`TestEveryTaskSourceFieldIsEditable`, which drives each key through the real dispatcher
  so a map entry naming a nonexistent one cannot pass), because a field settable only at
  creation means remove-and-re-add: retyping every other field and renumbering every later
  source to change one; **removal
  compares the WHOLE entry the caller listed**, never one field (several classifier rules
  share a situation, and one never-auto pattern is legitimately scoped twice — a one-field
  guard passes on the wrong element exactly when a listing has gone stale); and **an insert
  must respect the daemon's own lookup order** — `config.CaptureDelay` takes the first rule
  matching the agent type and `"*"` matches everything, so a specific rule appended after a
  wildcard one is configured, listed, and never read. Secrets are a DISPLAY rule, not an
  exemption: `hap config env` never prints a value and reads it from stdin unless `--value`
  is passed, so a token stays out of shell history and `ps`.
  **`[[task_sources]]` is the exception to that insert rule, and it inverts it: a new source
  is always APPENDED.** Its index is a public selector — `hap task 2 …`, the
  `{task_source_index}` a delivered prompt tells an agent to use, and the number every
  `hap config task-source` listing, `set` and `remove` takes — so inserting anywhere else
  silently re-points every selector after it, including commands already sitting in an
  agent's scrollback. That binds BOTH creating surfaces: `AddTaskSource` (CLI and the TUI
  Config tab) and `addTaskSourceIfAbsent` (accepting an LLM task suggestion, which registers
  a source as a side effect). Task sources need no lookup-order insert because they are
  matched by SELECTOR, not first-match-wins by position. Removal necessarily renumbers what
  follows, which is why the agent NAME stays the primary selector and the index is the
  fallback for sources a name cannot address. Keep the paired tests
  (`TestAddingATaskSourceNeverRenumbersTheExistingOnes` /
  `TestAcceptingAGeneratedTaskAppendsItsSource`).
- **Fail safe on the daemon path** — no panics; every error resolves to escalate + audit +
  log. Wrap new handler/adapter calls in `logging.Guard`.
- **Safety controls are never bypassed** — LLM submissions and learned rules alike are
  re-gated through kill switch, never-auto patterns, rate guard, and retry ceiling. Changes
  touching these must keep/extend the safety-invariant tests; new destructive-command shapes
  go in `internal/domain/testdata/irreversible_corpus.txt` (CI fails if seed patterns miss a
  corpus entry).
- **A multi-tab form's baseline is the swept AGGREGATE, so nothing may compare it to one
  frame** — the pane shows one question at a time, so `sweepFrames` walks every tab and
  `AggregateMCQFrames` joins them into the `Situation.Content` that mints the signature and is
  stored as the audit row's `pane_excerpt`. Anything re-reading the pane later holds a SINGLE
  frame, and the two can never hash alike. That is not a near miss to be absorbed by a
  tolerance: a `choice` salient is STRUCTURED, so `SignatureHeldStill` compares it exactly and
  the mismatch reads as proof the situation moved on. Auto-accept's Guard 3 did exactly this
  and auto-dismissed EVERY multi-tab escalation as `auto_dismiss_stale` — verified live
  2026-08-16, 22ms after the escalation was raised, against a form that stood for another 17
  minutes. Deterministic, never intermittent, and invisible because it looked like the feature
  working. So a frame is compared FRAME-WISE, the way `daemon.seriesStale` and
  `deliver/codex.go` already do: same tab count, and the live `ExtractAgentMCQForm` equal to
  one of `AggregatedMCQFrames(excerpt)`. Guard 3 widens that in two ways `seriesStale` does not
  need, because it runs after a WAIT rather than immediately before the keystrokes — every
  frame is a candidate (the operator may have tabbed around, and delivery resets to tab 1 with
  a Left-arrow burst anyway), and both sides go through `domain.NormalizeMCQFrame`.
  **Each widening buys a gate, and all four are load-bearing** — every one was proved by
  mutation, and three of them close hazards the widening itself created:
  - `NormalizeMCQFrame` folds the caret AND the preview box, because on a preview tab the box
    is a FUNCTION of the focused option and a digit only moves the caret — so hap's own failed
    attempt repaints the whole right column. Folding the caret alone fixes nothing there. What
    survives is header + question + each option's first line, which is what keeps two different
    forms unequal; the caller answers "held still" by typing a digit series into the pane.
  - The suggestion must be a series of exactly `AnswerCount` digits. Otherwise `deliver.Deliver`
    never takes the answer-series branch: it falls to the plain-menu path and maps the reply
    against **whichever tab is visible**. Such rows exist (`unfamiliar_options` rejects a
    wrong-shaped multi-tab LLM answer and leaves it pending WITH the answer attached, and that
    reason is not in `autoAcceptExcludedReasons`) and were simply unreachable while the guard
    always said no. Without the gate a bare label is delivered and the row reads `auto_accepted`.
    The token COUNT alone is not enough, because a token may be a comma group (`1,3`) that only
    a multi-select tab can take: `mcqdeliver.answerTab` does refuse one, but at THAT tab, so a
    comma group on any tab after the first is caught only after the earlier tabs are already
    answered and committed — a half-answered form, which is exactly what `verifyTabBaseline`'s
    all-or-nothing contract exists to prevent. The captured frames carry each tab's mode, so
    the shape is checked against `domain.MultiSelectTab(frames[i])` before anything is pressed.
  - `domain.MCQFormFullyUnanswered` — no `☒` in the live header. Answering a tab flips it while
    the form still stands, delivery resets to tab 1 and retypes EVERY tab, and an answered
    single-select tab has no `CheckedOutside` equivalent to catch it later. Since
    `NormalizeMCQFrame` folds the marks, this must be asked BEFORE the comparison, not left to
    it. Same doctrine as the checkbox baseline: a widened baseline needs evidence.
  - `domain.LooksLikeAggregatedMCQ` — excerpts are stored through `truncateTailRunes`, which
    keeps the TAIL and prefixes `…`, and the incident aggregate was 3606 runes against the 4000
    cap. One more tab and the `[question 1/N]` head is gone; treating "not a complete aggregate"
    as "some other capture" drops straight back into the whole-vs-frame comparison — the
    original bug, silently restored. A long enough FINAL frame leaves no marker at all, so the
    `excerptTruncationMarker` prefix is checked as well — it is then the only surviving evidence
    that the row is a mangled capture rather than a different kind of capture, which is why the
    marker is one shared constant with a test pinning writer and reader together.

  All four refuse as `heldStillUnevaluable` (PENDING), never `heldStillNo`: a malformed answer,
  a half-answered form and a mangled capture each need a human, not a dismissal. A row carrying
  no aggregate at all is the same (legacy only — retention never blanks these, since
  `PruneAuditExcerpts` excludes `escalated`/`auto_accepting` at any age). Keep the paired tests
  (`TestSweptAggregateStillMatchesItsLiveFrame` / `TestNormalizeMCQFrameFoldsAMovedCaretOnAPreviewTab` /
  `TestMCQFormFullyUnanswered` / `TestLooksLikeAggregatedMCQCatchesATruncatedCapture` /
  `TestAutoAcceptDeliversAStandingMultiTabForm` / `…DismissesADifferentFormOfTheSameSize` /
  `…HoldsAFormParkedOnALaterTab` / `…LeavesAMultiTabFormWithNoCapturePending` /
  `…RefusesASuggestionThatIsNotAnAnswerSeries` / `…LeavesAPartAnsweredFormAlone` /
  `…LeavesATruncatedAggregatePending` / `TestGuard3HoldsTheRealPreviewForm`), and note that
  every OTHER multi-tab fixture in this repo renders options without previews — the real
  captured pair in `internal/domain/testdata/mcq_preview_*.txt` is the only one that reproduces
  the layout, which is why this shipped green.
- **A swept AGGREGATE is the one capture whose HEAD is load-bearing, so it gets its own
  storage budget** — every other `pane_excerpt` is a pane TAIL (the prompt or error is at the
  bottom, old scrollback goes first), which is why `truncateTailRunes` keeps the tail and
  prefixes `…`. An aggregate is the opposite shape: `AggregatedMCQFrames` reads the tab total
  out of the FIRST `[question 1/N]` marker and demands N markers running 1..N, so shearing the
  head does not degrade the capture, it DESTROYS it — and `mcqFormHeldStill` then answers
  `heldStillUnevaluable` on every sweep, forever, at Debug level only. The row is neither
  delivered nor dismissed and nothing says why. Verified live 2026-08-18 (audit #1092): a 4-tab
  AskUserQuestion form with preview boxes stored 4001 runes with 3 of its 4 markers left, and
  sat pending across BOTH the FSP sweep and the 5-minute timed threshold until the operator
  cleared it by hand. The real 3-tab preview fixture is already 3606 runes, so 4000 never fit
  four tabs — this is every large form, not an edge case. Hence `aggregateMaxRunes`
  (`truncateExcerpt`/`excerptBudget`), and three bounds on it: the gate is a **strict parse**,
  never `LooksLikeAggregatedMCQ` (at write time content is untruncated so a real aggregate always
  parses, while the marker test would hand the big budget to any pane that merely PRINTED
  `[question 1/4]` — `LooksLikeAggregatedMCQ` stays the READER's evidence that a stored row is
  mangled); `SaveSignatureSnapshot` deliberately keeps 4000, because `signature_snapshots` has NO
  retention path at all and `hap signatures show` prints it unpaged; and
  `duplicatePendingEscalation` truncates with the new helper but still passes `snapshotMaxRunes`
  as the `snapshotCap` ARGUMENT, since there it is the "is this a large tail window" threshold
  that gates the two fuzzy dedup paths, not a storage budget — passing the bigger one would
  disarm them for exactly the captures most likely to need them. The refusal branches stay: this
  stops new rows being mangled, it does not make a mangled capture trustworthy. Note the test
  seeder `seedAgedSweptEscalation` was the only writer of that column anywhere that skipped
  truncation, which is why every existing test was blind to this; it now goes through
  `truncateExcerpt` like production. Keep `TestAnOversizeAggregateSurvivesTheCapturePath` /
  `TestAnOrdinaryCaptureKeepsTheSmallerBudget` / `TestOnlyARealAggregateEarnsTheBiggerBudget` /
  `TestAnAggregatePastItsOwnBudgetIsStillRefused`.
- **Full self-prompting may widen the YES side of Guard 3, never the NO side** — the mode gets
  two extra evidence paths, both reached ONLY after the ordinary comparison has already refused,
  both gated on the `fsp bool` already threaded through `autoAcceptOne`, and both required to
  resolve to `heldStillUnevaluable` (PENDING) when they decline. A fallback that could answer
  `heldStillNo` would turn a widening into a queue-destroying dismissal, which is the one
  outcome none of this may produce.
  - `mcqSalientHeldStill` answers the row whose CAPTURE was truncated past its head, using
    the blocks truncation did NOT reach. `truncateTailRunes` cuts from the top, so every block
    after the first surviving `[question k/N]` marker is byte-intact
    (`domain.SurvivingMCQFrames`, which stays structural: one declared total, consecutive
    indices, and the run must END at that total). Requiring the live frame to equal one of
    those blocks is the SAME frame-wise relation the intact path uses, just over a partial
    capture — and that relation is what makes this safe. **The option set alone is NOT
    sufficient identity and must never become the only gate**: it is the union over every tab,
    and every AskUserQuestion form ends in a generated `Submit answers`/`Cancel` tab, so a pane
    parked on ANY form's Submit tab is a subset of ANY other form's set — one form's answer
    series would be typed into a different form of the same tab count, exactly what
    `AggregatedMCQFrames` exists to prevent. `domain.LiveMCQMatchesSalient` is kept only as a
    cheap extra conjunct catching option drift, and it derives BOTH sides through
    `NormalizedOptionSet` then `MaskVolatile` — the stored salient was masked, so a live side
    normalized only for case and whitespace could never match a label carrying a path or a
    number, and would silently no-op for every such form. Three further gates are not optional:
    tab count must equal the answer-series length (or delivery falls to the plain-menu path and
    maps the reply against whichever tab is visible), the form must be fully unanswered
    (delivery resets to tab 1 and retypes EVERY tab), and **no token may be a comma group** —
    the tabs whose blocks are gone carry no select mode at all, so the shape cannot be verified
    for the whole form, and an unanswerable safety question is answered NO. A capture with NO
    surviving block (fully truncated, or a legacy row) has no identity evidence and stays
    pending: a deliberate limit, not an oversight. Liveness is still proved twice afterwards by
    `deliver.deliverSeries` and `mcqdeliver`.
  - `unstructuredHeldStill` answers a PANE-TAIL row using `domain.TailSimilarWithin`, which cuts
    both salients to the shorter one's length FROM THE TAIL before the trigram compare. The
    mismatch it exists for is structural, not statistical: the baseline is minted from a
    `--source recent` CONSUMING DELTA while every re-read is `--source visible`, so symmetric
    Jaccard is dominated by content only the longer side ever had and refuses however little
    moved — which is the documented reason idle and generated-task rows never auto-accept.
    Aligning on the TAIL rather than testing containment is what keeps it safe: a screen that
    moved on paints its new content at the BOTTOM, inside the compared window, while containment
    would answer "still there" with a new question sitting below the old one.
    `domain.MinTailCompareRunes` is the floor, and it is load-bearing for the same reason
    `embedding.min_salient_chars` is — two short tails compare equal whatever they say, making
    one near-empty screen a magnet. `fspTailHeldStillJitterPercent` is a SEPARATE constant from
    `staleDeferredSendJitterPercent` (shared by two other call sites) at the same value: the
    loosening is the alignment, not the tolerance. Because it runs AFTER `SignatureHeldStill`
    has refused, it must re-ask the two refusals its caller did not — either side over-masked,
    or a fresh salient that has become structured. An over-masked salient is mostly repeated
    `<path>`/`<num>`/`<hash>` placeholders, and two of those share almost every trigram, so they
    clear any tolerance over any window: the magnet failure arriving by a door the length floor
    does not cover.

  A third loosening sits outside Guard 3: under FSP a `@noop` suggestion is RETIRED
  (`ReasonAutoDismissNoop`) rather than left pending, because the sentinel means SEND NOTHING and
  can never become deliverable on a later sweep — so under a mode whose premise is that nobody is
  reading the queue it would sit forever. It is the only auto-accept path that acts with no pane
  evidence, and it can afford to be: nothing is typed, nothing is learned. It still honours the
  kill switch, the per-agent disable and the runaway-guard pause, and it makes `autoDismiss`
  reachable from FOUR places rather than three. Keep
  `TestFSPAnswersATruncatedAggregateWhoseLiveFrameSurvived` (the #1092 regression) /
  `TestFSPTruncatedAggregateRefusesADifferentFormOfTheSameSize` (the Submit-tab collision that
  is why the option set can never be the only gate) / `TestFSPRefusesACaptureWithNoSurvivingBlock` /
  `TestTimedAutoAcceptStillLeavesATruncatedAggregatePending` /
  `TestFSPTruncatedAggregateRefusesDriftedOptions` /
  `…RefusesEveryCommaGroup` / `…RefusesAPartAnsweredForm` / `…OverrideNeverDismisses` /
  `TestFSPUnstructuredFallbackRefusesAnOverMaskedPair` / `…RefusesANowStructuredLiveSalient` /
  `TestFSPAnswersAnIdleRowWhoseTailStillMatches` /
  `TestTimedAutoAcceptCannotEvaluateAMismatchedWindow` (the control proving the pair is not
  vacuous) / `TestFSPLeavesAMovedOnIdleScreenPending` / `TestFSPRetiresANoopEscalation` /
  `TestTimedAutoAcceptStillLeavesANoopEscalationPending` /
  `TestFSPNoopRetirementHonoursADisabledAgent`, and in `internal/domain`
  `TestLiveMCQMatchesSalient*` (including `…MasksBothSides`) /
  `TestNormalizedOptionSetRoundTripsThroughSplitOptionSet` / `TestSurvivingMCQFrames*` /
  `TestTailSimilarWithin*`.
- **The last look before a claim is COMPLETE, and a content-safety refusal is not a delivery
  fault** — everything above `ClaimForAutoAccept` (Guard 3's pane re-read especially) is a herdr
  shell-out with a budget in SECONDS, and the sweep walks every candidate, so the gap between
  "we checked" and "we send" is wide enough for an operator to act in. `claimBlockedBy` re-asks
  all three controls there, broadest first: the **kill switch** (FR-017 — not FSP-specific, so
  timed auto-accept re-reads it too, and it fails closed on a read error), the **mode** and its
  ceiling latch via `stillPermitted`, and **`accept_generated_task`** for a generated-task row,
  because that is a SEPARATE opt-in resolved once per sweep into `allowGenerated` — an operator
  turning off just that key mid-sweep would otherwise still have a task written and handed to an
  agent. `retireNoopEscalation` takes the same look, and the kill switch matters most there:
  Guard 1a returns before ANY dismissal precisely so pausing the herd never destroys the queue it
  protects, and a pause landing mid-sweep must get the same answer as one landing a second
  earlier. Separately, a refusal from the seam's at-send screen is tagged `errOutboundRefused`
  and handled like `errAgentDisabled` rather than as a delivery failure: it used to enter the
  retry budget, so the same never-auto match was refused `maxAutoAcceptAttempts` times and the
  row was then dismissed as `auto_accept_failed` — deleting from the queue exactly the escalation
  FR-015 says must always reach a human. The refusal is permanent by nature (the same text
  screens the same way every sweep), so a budget could only ever end in that dismissal. Reachable
  only through a custom `next_task_template`, since the daemon's pre-check renders with the
  DEFAULT one. Keep `TestFSPGeneratedTaskSafetyRefusalIsNeverDismissed` (which sweeps past the
  attempt budget — the single-sweep test could not see it) /
  `TestFSPRechecksAcceptGeneratedTaskBeforeClaiming` /
  `TestAutoAcceptRechecksTheKillSwitchBeforeClaiming` (both flavours).
- **Every auto-accept refusal names itself once** — `notePending` logs at INFO per (row, reason),
  cleared on delivery and pruned with the other per-row state. Before it, every "leave it
  pending" path was Debug-or-silent (Guard 1b and the pane-busy skip produced no output at ANY
  level), which is why #1092 took a five-round investigation and a live database query to
  explain. Once per (row, reason), not per sweep, because the pass re-examines every pending row
  every minute — but a row whose reason CHANGES is new information and says so. The call sites
  all sit AFTER `stillEligible` is set: an INELIGIBLE row `continue`s before that, so logging one
  there would be pruned every tick and re-logged every minute forever. Keep
  `TestAPendingEscalationSaysWhyExactlyOnce` / `TestAChangedPendingReasonIsReported`.
- **An option label stops at the preview column** — Claude renders options WITH previews in two
  columns (option list left, preview box right) and a pane is one flat text grid, so the box
  lands inside the option's own line. Line-anchored parsing therefore made option 1's label
  carry the box's top edge and gave an option whose text WRAPPED a label made entirely of
  preview content — labels that then changed whenever the highlighted preview did, so the same
  question minted a new signature on every caret move. `ParseNumberedOptions` cuts at the box
  column (`trimPreviewColumn`). The cut only ever removes a SUFFIX, which is what keeps it away
  from the checkbox rule below: a `[ ]`/`[✔]` marker is a PREFIX and always survives it, and a
  label that is nothing but preview text carried no checkbox to lose. A bare `─` is excluded
  from the glyph set on purpose — agents draw full-width separator rules with it.
  **A label that trims to nothing is recovered from the next row, never dropped**
  (`wrappedOptionLabel`): a long enough label starts BELOW its number, leaving that number's row
  carrying only the box, and dropping it loses a choice the agent is really offering — the
  option set reads one short and a reply of "2" maps to nothing. Recovery takes the FIRST
  continuation row only, gated on that row carrying a preview column, because an ordinary menu's
  indented line under an option is a DESCRIPTION and not part of the label (the repo's own 3-tab
  fixture renders that way, so getting it wrong would rewrite every existing choice signature).
  Wrapped rows are deliberately NOT stitched into one label: a terminal wrap is lossy —
  `…enabl`+`ed` is a mid-word cut needing no separator while `…the very`+`top` lost a space to
  the break, and measured on the real capture the two differ only by how near the gutter the
  text ends (4 columns versus 6), which is a property of one terminal width, not a rule. One row
  per label is stable, deterministic and never invented. Keep
  `TestWrappedPreviewOptionIsNotDropped` / `TestWrappedLabelsAreNotStitchedTogether` /
  `TestOrdinaryMenuDescriptionIsNotAnOptionLabel`.
- **A checkbox tab is answered by TOGGLING, so its baseline is a safety control** — a digit
  flips a `[ ]`/`[✔]` box rather than selecting it, so pressing one blind is not idempotent:
  over a pane already carrying an attempt's toggles it CLEARS them and the advance submits an
  empty answer. The rule is `checked ⊆ chosen`, enforced at DELIVERY (`domain.CheckedOutside`
  in `daemon.reverifyMultiSelect`, `frontend.verifyTabBaseline`, and again per keystroke in
  `mcqdeliver.toggleTab`, which presses only the missing boxes): hap's own boxes may already
  be set, anything else is the operator's and is never cleared. CAPTURE only records — refusing
  there would strand every form hap itself half-answered, since the next attention event
  re-captures that pane. The signature folds the checkbox state away
  (`domain.NormalizedOptionSet`) so a half-delivered form still matches the rule learned for
  the untouched one. **The widened baseline needs evidence**: it applies only when this daemon
  recorded its own attempt at this pane+signature (`markToggleAttempt`, in-memory, cleared on
  a completed delivery and lost across restarts — both fail safe). Without it a tab must be
  completely clean, because "checked ⊆ chosen" alone would also accept an operator halfway
  through ticking that very form. Keep all three invariant tests when touching this
  (`TestMultiTabSweepMultiSelectOwnTogglesComplete` / `…ForeignSelectionEscalates` /
  `…UnattributedTogglesEscalate`).
- **An unattended task hand-out is only "delivered" once the agent works** — a successful
  `agent send` proves herdr took the keystrokes, not that the agent acted on them, so the
  `[-]` written at delivery is recorded in `task_reservations` and confirmed only by a
  `working` transition (`daemon.handleTransition`). `daemon.reclaimStrandedTasks` returns an
  unconfirmed item to `[ ]` once its agent is parked again past `reclaimGrace`, which is what
  makes each sweep decide from current state rather than a past send. Four bounds are
  load-bearing and must survive any change here: the daemon releases ONLY a `[-]` it holds a
  ledger row for (an operator's or an agent's own mark is never cleared); **one unconfirmed
  hand-out per agent** (`agentsAwaitingHandout`), because confirmation is per-agent and a
  second hand-out would let one resumption confirm — and so strand — the untaken first;
  confirm and reclaim both compare `terminal_id`, since herdr recycles pane ids and an agent
  id IS a pane id; and an item handed out `maxTaskHandouts` times without ever being started
  is left `[-]` and escalated instead of resent forever. Keep the invariant tests in
  `autosendidle_test.go` (`…ReclaimsStrandedHandoutAndResends` /
  `…ConfirmedHandoutIsNeverReclaimed` / `…ReclaimIgnoresForeignInProgressItems` /
  `…OneUnconfirmedHandoutPerAgent` / `…RecycledPaneCannotConfirmItsPredecessorsHandout` /
  `…HandoutCapEscalatesInsteadOfResending`).
- **Full self-prompting's two opt-in keys are OFF by default, and each buys one narrow
  thing** — `full_self_prompting.honour_limits` and `…accept_generated_task`
  (`config.FullSelfPrompting`). Four bounds are load-bearing:
  - **The ceiling check must not read a PAUSE as a ceiling.** `domain.CheckRate`'s first
    branch answers `rate_limited` for a paused agent, but `fspCeilingReached` runs BEFORE
    Guard 1b (`autoAcceptAgentSuppressed`), which already suppresses paused agents
    correctly. Passing the rate through unmodified would let one operator-paused agent —
    or a leftover rate-pause from the ordinary decision path — switch the mode off for the
    whole herd over a state nothing new happened in. The rate is copied with `Paused`
    cleared, and only the two counters decide.
  - **The stand-down latches in memory FIRST, writes config second, and the write runs off
    the select loop.** `App.UpdateConfig` nudges the daemon's own control socket for the
    reload, so an inline write would block that loop on a round trip to itself. The latch is
    what makes the sweep that noticed stop immediately, is checked inside `fspActive` so both
    the sweep and the escalate-time hook honour it through one gate, de-duplicates the write
    and the operator notification to one per ceiling, and is cleared on reload so a re-enable
    takes effect (the rate row is untouched, so a ceiling that still stands re-trips at once).
  - **The attribution is written at FINALIZE, and the finalize RETRY must carry it.**
    `retryAutoAcceptFinalize` re-calls `MarkAutoAccepted` on a later tick with only the id,
    so `autoAcceptNeedsFinalize` is `map[int64]bool` (id → was-FSP), not a set — a set drops
    the flag on exactly the rows whose bookkeeping already failed once. Claim time is NOT an
    option: `ReclaimAbandonedAutoAccepts` returns abandoned `auto_accepting` rows to
    `escalated` at startup and would strand a true flag on a row nothing ever delivered. The
    store ORs the column rather than assigning it, so a replay can only ever set it.
  - **A generated task is screened TWICE, and the second one is the real gate.** The task text
    is authored by the generator LLM AFTER the decision that raised the escalation, so no
    safety control has ever seen it — `handleTaskGenOutcome` validates only the shape, because
    the operator's confirm was the gate. This feature removes that gate, so the never-auto and
    suspected-irreversible screens run in the fork, before the seam, on the RENDERED
    `DeclaredTask.Prompt()` rather than the stored text (stored items keep line breaks as the
    literal two-character `\n`, which a line-anchored rule cannot match while the real newline
    reaching the pane can — screening the stored form fails OPEN, the same trap
    `tasklistreview` documents). A hit reverts the claim and leaves the row escalated, because
    FR-015 says a never-auto match always reaches a human.
    The daemon-side check (`generatedTaskUnsafe`) can only render with the DEFAULT template —
    the target source, and so its own `next_task_template`, resolved path and index, is chosen
    inside the seam — so a custom template could frame a benign task into something the rules
    refuse and still pass it. The seam is therefore handed a `screen func(string) error`
    (the same shape as `tasklistreview`'s `safe` closure) and calls it with the EXACT prompt
    immediately before the send, BEFORE the reservation so a refusal strands nothing. The
    pre-check earns its place by refusing obvious cases before any list is written. Note the
    daemon's own tests drive a FAKE seam, so only a `frontend` test can prove the real path
    calls the callback: `TestAutomatedGeneratedTaskScreensTheSourceTemplatePrompt`.
  - **A generated-task acceptance is the DAEMON's row to finalize.** `autoAcceptOne` has
    already claimed it (`escalated → auto_accepting`), so the `automated` flag makes
    `frontend.acceptGeneratedTask` skip BOTH `ResolveEscalation` (whose escalated-guard
    could now only fail, after the files were written) and `InsertCorrection` — an automatic
    acceptance must never feed the confidence model, which is the entire reason
    `AuditStatusAutoAccepted` exists apart from `resolved`. The seams
    (`Options.AcceptGeneratedTask` / `DisableFSP`) are optional function fields wired in
    `cmd/hap`, so `internal/daemon` still does not import `internal/frontend`; a nil seam
    returns the claim rather than stranding the row in the transient status.
    **EVERY status check on that path has to know it**, including the cheap early-out above
    the claim (`audit.Status != "escalated"`), and missing THAT one is destructive rather
    than merely broken: the error propagates to `autoAcceptDeliveryFailed`, which burns one
    attempt per sweep and DISMISSES the escalation once the budget is spent — so the feature
    would delete the very suggestions it exists to act on, minutes after each is raised. It
    shipped green because the daemon tests wire a FAKE seam; only a `frontend` test driving
    `AcceptGeneratedTaskAutomatically` against an already-claimed row can catch this class
    (`TestAutomatedGeneratedTaskAcceptsAClaimedRow`, proved by mutation). The seam call
    also runs INSIDE `WithAgentAutomation`, like every other delivery: it sends the first
    task to the pane, so a disable landing after Guard 1b must still stop it. Safe against
    the config lock the seam takes on its way through `addTaskSourceIfAbsent` — the barrier
    is a per-agent flock and the config lock a separate file, and no path takes them in the
    opposite order. `TestFSPGeneratedTaskTakesTheLifecycleBarrier` asserts the barrier is
    TAKEN rather than that a disabled agent is skipped, since Guard 1b makes the latter pass
    either way.
  - **An unattended hand-out gets a ledger row; an operator's does not.** The reservation
    marks the item `[-]` before the send, and a crash in that window used to strand it: the
    audit row is reclaimed to `escalated` at startup but the checklist marker is not, so the
    retry reads the item as already taken, burns its attempt budget and the escalation is
    DISMISSED. `recordAutomatedReservation` writes the same `task_reservations` row the
    daemon's own hand-outs use, so `reclaimStrandedTasks` returns the item to `[ ]` once its
    agent is parked past the grace window; a rolled-back send retires it again. The operator
    path deliberately records nothing — a human is present to read the error, and ledger rows
    there would start barring manually-confirmed agents from the idle poll.
  - **The mode is re-asked immediately before the claim** (`stillPermitted`). The guard chain
    above it does pane READS with a budget in seconds, so an operator switching the mode off
    mid-chain would otherwise still get the send that follows; `WithAgentAutomation` covers
    the per-AGENT disable at delivery, not the global mode.
  - **The ceiling is only read on a row that could actually be DELIVERED.** `ConsecutiveAuto`
    is reset only by human interaction, so an agent that saturated it and was then killed
    carries it forever — and its leftover escalation is still a candidate. Reading that as a
    ceiling stands the mode down for the whole herd over a row nothing could have sent, and
    because the latch clears on reload it re-trips after every operator re-enable, so the
    mode can never stay on again; an early exit also skips `autoAcceptOne`, so the absence
    bookkeeping that would eventually retire that row never advances and the livelock has no
    end. Hence the `live`/`autoAcceptParked` gate before the check, and `continue` rather
    than `break` after a stand-down so the remaining candidates still register in
    `stillEligible` (a `break` hands them to `pruneAutoAcceptState`, silently resetting
    delivery budgets and absence counts). The two ceilings are tested SEPARATELY rather than
    through one `CheckRate` call so the stand-down can name which tripped — and the
    per-minute one honours the window rollover, or a count from a window that elapsed
    minutes ago would switch the mode off over traffic that has stopped. Keep
    `TestFSPCeilingIgnoresAnUndeliverableRow` / `…IgnoresAnAgentThatWentBackToWork` /
    `…StandDownKeepsLaterCandidatesAccounted` / `TestFSPCeilingNamesWhichLimitTripped` /
    `TestFSPPerMinuteWindowRolloverIsNotACeiling`, and for the three above
    `TestFSPGeneratedTaskScreensTheExactOutboundPrompt` /
    `TestAutomatedGeneratedTaskScreensTheSourceTemplatePrompt` /
    `TestAutomatedGeneratedTaskRecordsAReservation` / `TestOperatorConfirmRecordsNoReservation` /
    `TestAutomatedGeneratedTaskReleasesTheReservationOnAFailedSend` /
    `TestFSPRechecksTheModeBeforeClaiming`.

  Accepted limitation, not a bug: a generated-task escalation is `idle`-typed, so its
  baseline salient is unstructured pane-tail and Guard 3 usually answers
  `heldStillUnevaluable` — the row waits for the operator. Keep the paired tests
  (`TestFSPHonourLimitsRefusesAtTheCeiling` / `…OffKeepsTodaysBehaviour` /
  `…IgnoresAPauseThatIsNotACeiling` / `…SwitchesOffOnlyOnce` /
  `TestFSPCeilingLatchStandsTheModeDownImmediately` / `TestFSPRefusesAGeneratedTaskByDefault` /
  `TestFSPAcceptsAGeneratedTaskWhenEnabled` / `TestFSPGeneratedTaskWritesNoCorrection` /
  `TestFSPGeneratedTaskWithNoSeamLeavesItPending` /
  `TestFSPGeneratedTaskNeverAutoTextStaysPending` /
  `TestFSPGeneratedTaskScreensTheRenderedForm` / `TestFSPFinalizeRetryKeepsTheAttribution` /
  `TestMarkAutoAcceptedNeverClearsTheFlag`).
- **`enable_auto_send_task_when_idle` skips the LEARNING gates, never the safety ones** —
  a declared task from a source with that flag (`DeclaredTask.Reserve`) resolves ahead of any
  learned noop precedence (`resolveSituation`) and bypasses BOTH the shadow-mode gate and the
  confidence gate (`domain.Decide`, keyed on the existing `idleHandout` predicate — the
  VERIFIED classified situation plus the RESOLVED action, never a sweep-time flag). The flag
  is an operator instruction about a QUEUE; a learned action is an inference about a SCREEN,
  and every idle screen mints its own signature, so holding the queue to per-signature
  graduation means the feature never delivers unattended — which is the only thing it is for.
  The bypass sits AFTER the variance guard, rate guard and suspected-irreversible heuristic
  and before nothing else; the kill switch, never-auto patterns, per-agent disable and the
  optional pre-delivery LLM review all still apply at delivery. Sources without the flag keep
  the historical behavior exactly. Keep the paired invariant tests
  (`TestDecideUnattendedSourceSendsWithoutGraduating` / `…OutranksALearnedNoop` /
  `…StillObeysEverySafetyControl` / `TestDecideAttendedSourceStillWaitsToGraduate` /
  `TestAutoSendIdleUnattendedSourceSendsWithNoLearnedRule` / `…AttendedSourceStillEscalates…`).
- **A pending escalation never benches an agent from the idle poll** — `eligibleIdleAgents`
  has no escalation gate, deliberately. An escalation is a question about what to answer on
  the agent's SCREEN, not a verdict on whether it can take its next declared task, and gating
  on one deadlocks the feature against itself: a pending task is what raises
  `noop_vs_pending_tasks`, which then blocks the poll that would deliver it. Do not reach for
  the audit `Trigger` to tell "the poll's own" escalations apart either — `daemon.trigger`
  derives it from `tr.AutoIdleSend`, so EVERY escalation raised on a poll-driven episode is
  stamped `auto-idle-send:`, which is the common shape for exactly the parked agents this
  feature exists for. **The bound on an undeliverable task is per ITEM, not per agent**: a
  failed send rolls its item back to `[ ]` and records NO reservation, so
  `reclaimStrandedTasks` can never age it — `deliverAutonomousClaimed` therefore counts the
  attempt itself (`RecordTaskHandoutAttempt`) and at `maxTaskHandouts` skips the rollback,
  leaving the item `[-]` and escalating (`escalateUndeliverableTask`). Because the poll
  ignores `episodeHandled` on purpose, an agent whose episodes keep NOT sending is otherwise
  re-read every sweep forever, so `Daemon.pollRedrive` widens the interval (1, 2, 4 … capped
  at 15m) and any delivered send clears it — a delay, never a bench. Keep the invariant
  tests (`…OrdinaryEscalationDoesNotBlockHandout`, whose poll-raised cases pin the trigger
  trap / `…UndeliverableTaskIsCappedNotRetriedForever` / `…CapsAtExactlyMaxHandouts` /
  `…CappedTaskLetsTheAgentMoveToTheNextItem` / `…EscalatedAgentStillGetsItsNextTask` /
  `…BacksOffRedrivingAnAgentThatNeverSends` / `TestTaskReviewFailedSendCountsTheHandoutAndRollsBack`).
- **An inherited task-source provider is a live link, never a snapshot** — `[task_source_provider]`
  is the DEFAULT storage backend and each `[[task_sources]]` entry may override it with its own
  `provider`/`gist_id`. An empty per-source key IS the inheritance and must NEVER be materialized:
  `normalizeTaskSources` runs on **Load AND Save**, so filling it in "like we do for `max_tasks`"
  would stamp the current default onto every existing source the first time any surface writes
  config — and the operator's later `hap config set task_source_provider.provider …` would then
  move nothing, silently, for every install that already had a config. It fails no obvious test;
  `TestInheritedProviderIsNeverMaterialized` is the one that catches it. Provider misconfiguration
  is detected at USE time (`ValidateResolvedProvider`), never at `Load`, and never coerced —
  coercing an unrecognized provider to `local_fs` would resolve gist-shaped names against the
  daemon's cwd and CREATE local checklist files while the operator believed their lists were
  remote. A missing `gist_id` is deliberately not a write-time rule either: it would make the keys
  order-dependent to set (`TestValidateTaskSourceAcceptsAMissingGistID` pins the omission).
- **A task-list locator is never handled as a FILE outside the local backend** — `taskfile.Mutate`,
  `MutateWithin` and `WriteFileAtomic` take a path; a locator is not one. Under a remote provider it
  is a `gist://<id>/<file>` URI that `os.Stat` can only fail on, and the failure reads like a missing
  file (`stat gist://…: no such file or directory`) rather than like code looking in the wrong place.
  Every checklist mutation therefore goes through the STORE — `frontend.mutateList`/`mutateTask`,
  `daemon.mutateTaskList` — and there is deliberately NO local `mutateTaskFile` alias in
  `internal/frontend` to reach for. `internal/taskstore/local` is the one exemption, because there a
  locator IS a path. The hazard is that the local twin of a store method passes every test: the
  generated-task confirm moved its read and write to the store and left the send-time RESERVATION on
  `taskfile.Mutate`, so under `github_gist` it created the list, registered the source, CLAIMED the
  escalation and only then died with nothing sent. Reading is the same trap by a different call:
  `pickAppendTarget` chose among an agent's matched sources with `os.ReadFile`, which cannot resolve a
  gist source's bare file name, so it silently degraded from "the source with pending work" to "the
  first source" — wrong list, no error. The bootstrap EXCLUSION had it a third time: it compared that
  same absolutized path against the local bootstrap path, so under a gist provider an agent's own
  list read as external and a second confirm took the append path, whose dedupe keys on raw text
  while the bootstrap flow stores NUMBERED items — appending every listed task again, un-numbered.
  Comparison is therefore on canonical LOCATORS (`isBootstrapList`) — **plus a declared path**, and
  that second half is not optional: a DERIVED source (`path` unset, the documented one-list-per-agent
  form) resolves to `DerivedFileName(agent)`, byte-identical to what the bootstrap registers, so
  locator equality alone excludes the most idiomatic gist setup, runs the bootstrap flow, and
  `addTaskSourceIfAbsent` then refuses to register a second source for the agent — failing every
  retry, after the tasks were already written. Never compare the declared spelling ALONE either: the
  same file name in a different gist (`gist_id` override) is a different list. The bootstrap
  locator's own resolution error is likewise DEFERRED past the append early-return, because the
  append path never needed it — a misconfigured remote default beside a healthy `local_fs` source
  must still work. Two guards, and they cover
  different halves: `TestOnlyTheLocalBackendTouchesATaskListAsAFile` bans the path-taking MUTATORS
  by construction (matched on the selector, so a function value is caught too, and a dot-import of
  `taskfile` is refused outright since it would make the ban unenforceable; exemptions carry a
  reason and are checked live), while the READ half needs behavior — hence `App.TaskStoreFor`, the
  test-only seam that stands a remote backend up in-process. **Every frontend test before it ran on
  a local file, where a locator IS a path, which is the single reason all of this shipped green** —
  so a change here belongs in `remote_confirm_test.go`, against a store that refuses a non-gist
  locator. Keep `TestRemoteConfirmReservesThroughTheStore` /
  `…RollsBackThroughTheStore` / `TestRemoteAppendTargetReadsCandidatesThroughTheStore` /
  `TestRemoteSecondConfirmDoesNotDuplicateItsOwnList` /
  `TestAppendTargetPrecedenceSurvivesTheStoreRewrite`. A compensating write (the reservation
  rollback) additionally takes `context.WithoutCancel`: a remote store uses the caller's ctx, and
  the likeliest cause of the failed send it undoes — the operator quitting — is the same
  cancellation that would abort the release and strand the item at `[-]`.
- **A task-list locator is canonicalized in exactly one place** — `tasklocator.Canonical`, which
  `taskfile.LockPath` and both `canonicalTaskPath` copies delegate to. A scheme'd locator
  (`gist://…`) is returned verbatim, and the hazard of a second copy forgetting that is SILENT:
  `filepath.Abs` does not fail on one, it returns `<cwd>/gist:/id/file`, and each hap process has a
  different cwd — so the daemon's claims and the TUI's grouping would stop agreeing with nothing
  erroring. Each of the three packages has a test pinning the delegation.
- **A task list is never created BLANK** — GitHub cannot store a blank gist file at all: `""`,
  `"\n"` and `" "` are all refused with `422 Validation Failed {Resource:Gist Field:files
  Code:missing_field}` (verified live 2026-08-12), because a blank body reads as "this entry
  carries no file", the entry is dropped, and the request is then rejected for an empty `files`
  map. So the error names `files` — never the list, never the content — and nothing is created.
  `newListHeader` is the ONE seed every create-on-demand path passes to `ensureList`, and the
  rule is enforced in THREE places on purpose: `frontend.ensureList` refuses a blank `initial`
  for EVERY backend (a local file takes one happily, so guarding only where it breaks means the
  next caller is green through the whole unit suite and fails for the first `github_gist`
  operator — exactly how the generated-task confirm shipped seeding `""`, making accepting an
  LLM-suggested task impossible); `ports.EnsureCreator` states it as the interface's contract;
  and `gist.Store.put` refuses with `ErrBlankContent` on EVERY write path, not just a create, so
  a mutation that empties a list reports what is wrong instead of the 422. Only the CREATE is
  closed — a mutation may still empty a LOCAL list, which is what removing the last item from a
  headerless one does. The unit suite could not catch any of this, so `fakeGist` now answers a
  blank write with GitHub's real 422; a fake that accepts one is what let this through. Keep the
  paired tests (`TestGistRefusesToWriteBlankContent` / `TestGistFakeRejectsBlankContentLikeGitHub` /
  `TestBootstrapSeedsTheListWithItsHeaderNotBlank` / `TestNewListHeaderIsNeverBlank` /
  `TestEnsureListRefusesABlankSeedOnEveryBackend`).
- **Egress has exactly two exceptions, both opt-in and off by default** — the release check
  (`internal/updatecheck/fetch.go`) and the `github_gist` task-list backend
  (`internal/taskstore/gist/gist.go`), which carries task text only. `internal/privacy` bans the
  GitHub SDK by its own import path as well as `net/http`, because the walker checks DIRECT
  imports: an adapter using only the SDK would egress while passing. The gist adapter must keep
  using `github.WithURLs` (not a `*url.URL`) and `github.WithTimeout` (not a hand-built
  transport), or `net/url` and the no-remote-dial scan need widening too.
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
  it disk-backed). Embed calls are stall-guarded and latch a degraded mode after 5 consecutive
  failures (a fixed constant, not configurable).
- **A short PANE-TAIL salient is never embedded — on EITHER side of the comparison** — below
  `embedding.min_salient_chars` (default 100, on the masked salient) matching uses BM25 instead.
  **STRUCTURED salients are exempt at any length, and that exemption is load-bearing**: they are
  short by construction (`permission:proceed | options:no;yes` is 35 chars), so a floor over them
  would switch cosine matching off for every approval, choice and error rule — the paraphrase
  matching the feature exists for. They are already guarded on their own terms
  (`ApprovalRemapCompatible`, `StructuredSalient`). If every pre-existing semantic test needs a
  lowered floor to pass, the floor's scope is wrong — that was the tell the first time.
  Sentence embeddings are not discriminative on a few generic tokens: any two near-empty screens
  land above `similarity_threshold`, so ONE almost-empty learned rule becomes a magnet that
  silently answers every unrelated situation. `domain.EmbeddableSalient` is the single definition
  and is enforced three times, because closing only the query side still lets a long screen match
  a short stored rule: the incoming situation skips the embed call, a newly minted short rule is
  persisted with no vector, and an existing short rule is stripped of its vector by
  `reembed.Reconcile` AND vetoed again in `resolveSignature`'s vector accept filter (the veto
  covers the window before a rebuild — an index built by an older build, or a row added under a
  lower floor). Reconcile runs at every daemon start and `[embedding]` reload, which is what heals
  an existing database with no migration. Such a rule stays reachable by BM25 and exact hash; only
  cosine is closed to it. Keep the paired tests
  (`TestResolveSignatureShortSalientSkipsEmbedding` / `…ShortStoredRuleIsExcludedFromVectorSearch` /
  `…ShortStoredRuleStillMatchesByText` / `TestReconcileShortSalientRowsAreStrippedOfVectors`).
- **Agent-TUI chrome is redacted from pane-tail salients, gated on agent type** —
  `domain.StripClaudeChrome` (banner, `───` rules, spinner/token-counter line, `⏵⏵` mode line,
  herdr status bar, trailing `❯` composer) for claude; `domain.StripCodexComposer` for codex.
  Chrome is byte-identical across unrelated panes, so it BOTH inflates similarity between
  different screens and eats the `pane_salient_chars` window. The strip runs in `salientContent`
  BEFORE the window is taken, and only on the pane-tail branch — structured salients return
  earlier. It only ever deletes lines it can positively identify; an unrecognized line is kept, so
  two different screens stay different (fail-safe). The `❯` filter is anchored on "last non-empty
  line" because `❯` is also an option-list caret — never widen it to the bare glyph. Every filter is
  ANCHORED for the same reason (leading spinner glyph, leading mode glyph, banner glyph at line
  start): a bare substring test deletes a whole line when the agent merely QUOTES the phrase, and
  the footer window is the entire capture on a short pane. The status bar needs three pieces of
  evidence together (>=3 pipes, no leading `|`, and the terminal-width padding run before its
  trailing token) — the pipe count alone also matches a shell pipeline the agent reported running.
  The banner filter likewise needs positive evidence, not position: it is ARMED only when the head
  of the capture carries the `Claude Code` marker, and each line must hold >=2 CORNER glyphs
  (`▐▛▜▌▝▘`, which `█` is not). A capture does not guarantee the logo is on screen — `--source
  recent` is a consuming delta and a scrolled pane starts mid-output — so `████████ 80% done` can
  legitimately be line 1, and stripping it would collapse two screens differing only in bar length.
  Accepted trade-offs: a status bar rendered WITHOUT a trailing token is not recognized (chrome
  survives into the salient — degraded, never dangerous), and a pane left with only a word or two
  after the strip trips the over-masking floor and escalates.

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
- **herdr 0.7.5 REMOVED `agent send`**, and nothing replaces it one-for-one — the old call now
  exits 2 with a usage banner and nothing reaches the agent. `agent send` quietly did two things
  and the survivors split them, so `internal/herdr.CLI.submitText` **routes on the content**:
  - **single-line → `pane send-text` + `pane send-keys enter`.** Literal terminal input, so a
    menu digit arrives as the KEY it is. This is safety-critical: hap answers an approval by
    mapping the option to its digit (`domain.MenuKeystroke`), and verified live (2026-07-31)
    against a real Claude question form, `agent prompt "2"` PASTES the 2 as text and its Enter
    commits whichever option the caret was on — it answered "Apple" while hap had chosen
    "Banana", silently, with a success exit code. Never route a digit through paste.
  - **multi-line → `agent prompt`.** Writes the text AND its Enter in one request honoring the
    pane's live bracketed-paste mode, so a task hand-out lands as ONE message. `pane send-text`
    is NOT paste-aware — each embedded newline is a literal Enter, which submits the first line
    and types the rest into the next prompt.

  Both fall back to the legacy `agent send` (+ Enter) only on exit status 2 — herdr rejecting the
  VERB — which keeps `min_herdr_version = 0.7.0` honest. A pane-level failure exits 1 with a JSON
  error body and is returned as-is, so a real delivery error is never retried as a second send.
  Keep the paired tests (`TestSingleLineSendTypesTheTextSoAMenuDigitSelects` /
  `…NeverPastes` / `TestMultiLineSendPastesAsOneMessage`).
- **`pane send-keys shift+tab` is ACCEPTED and delivers a bare TAB.** Verified live
  (2026-08-09, herdr 0.7.5): herdr validates the key name, exits 0, and writes `0x09` —
  the shift modifier is dropped. Proved by sending it to a pane running `cat -v`, where
  `shift+tab` and `tab` produced byte-identical output, and by both Claude Code and Codex
  ignoring it across repeated presses while every send reported success. `backtab`, `btab`
  and `S-Tab` are all rejected outright (`invalid_key`), so there is no key NAME that
  works. The chord must be written as its raw terminal encoding, CSI Z (`domain.ShiftTab`
  = `"\x1b[Z"`), through `pane send-text` — which is the right transport precisely because
  it is not bracketed-paste aware, so the bytes pass through untouched
  (`herdr.CLI.SendChord`, `ports.ChordSender`). This is the reason
  `frontend.SetAgentMode` is an open loop that re-reads the pane after every press: a
  green exit code from herdr is not evidence a chord landed. Keep the paired tests
  (`TestSendChordTypesTheRawEscapeAndNeverSubmits` /
  `TestShiftTabIsTheRawEscapeNotAHerdrKeyName` /
  `TestSetAgentModeGivesUpOnADeafAgent`).
- **An agent's permission mode is READABLE ONLY FROM ITS PANE, and only positively.**
  Neither `agent list` nor `pane get` carries a mode field, so `domain.AgentModeFromPane`
  parses the indicator the agent paints in its composer footer. Two rules are
  load-bearing. **Absence is UNKNOWN, never a default**: every Claude mode renders a line
  (verified live against 2.1.226 — including `⏸ manual mode on`, which uniquely omits the
  `(shift+tab to cycle)` hint), so a capture with no line is a capture that does not show
  the footer. **Matching is on the LABEL, never the glyph**: `accept edits on` and
  `auto mode on` both render `⏵⏵`, so a glyph-keyed parser cannot tell the most permissive
  mode from the middle one. Codex is the mirror image — it appends a right-aligned
  `Plan mode (shift+tab to cycle)` to its `model · cwd` footer in Plan mode and nothing at
  all in Default — so "no segment" only means Default once the footer itself is
  recognized.
- **The mode cycle is per-SESSION, not per-agent-type, so a set must detect a closed
  rotation.** Verified live (2026-08-09): a `--model haiku` Claude session rotates through
  only three modes — manual, acceptEdits, plan — while a default-model session in the same
  build offers all four. `domain.AgentModesFor` is therefore a SUPERSET, never a promise.
  `frontend.SetAgentMode` tracks the modes it has observed and stops the moment the
  rotation returns to one, because the naive alternative is not merely a worse error
  message: pressing to the ceiling leaves the agent parked in an arbitrary PERMISSION mode
  nobody asked for. A failed set therefore also ROTATES THE AGENT BACK to where it started
  (`restoreMode`). Note the two diagnoses are distinct — a mode that did not change at all
  means the chord did not land and must keep pressing to the ceiling; only a mode that
  CHANGED into one already seen means the cycle closed. Keep the paired tests
  (`TestSetAgentModeDetectsAModeThisSessionDoesNotOffer` /
  `TestSetAgentModeGivesUpOnADeafAgent`).
- **Shift+Tab is REBOUND inside Claude's modals, so a mode press needs positive composer
  evidence.** A standing plan approval renders `shift+tab to approve with this feedback`
  (see `internal/classify/testdata/transcripts/approval_claude_plan.txt`), so pressing the
  chord there APPROVES THE PLAN. `domain.ClaudeComposerReady` therefore requires the
  composer SANDWICH — a `───` rule, the `❯` input line, and a second rule below it — not
  the bare `❯`, which is also the caret an option list draws in front of its highlighted
  choice. Refusing merely because a known form was *detected* is not enough; the ordinary
  composer must be *proven*. Readiness is re-checked before EVERY press, not once up
  front, because a prompt can appear between two presses. Keep the paired tests
  (`TestClaudeComposerReadyRefusesAStandingApproval` /
  `TestSetAgentModeRefusesAModalThatAppearsMidRotation`).
- **A herdr agent name is 1-32 chars of `[a-z0-9_-]` starting with a lowercase letter**
  (`invalid_agent_name`), and `agent start` refuses a name already in use. Integration cases
  therefore derive a unique short name from `t.Name()` — a shared one made whichever case ran
  second fail to start.
- **`agent prompt` needs the agent to be interactively READY, and says so.** Verified live
  (2026-07-31): a prompt issued in the seconds after `agent start`, or while claude still shows
  its release-notes screen, lands in the composer WITHOUT submitting. The status-gated
  retry-Enter loop in `CLI.send` is what recovers that, so do not remove it on the grounds that
  submission is atomic now. A pane whose agent is not the foreground process is refused outright
  with `agent_not_ready` — which is why an externally reported agent (`pane report-agent` over a
  bash stand-in) can never receive `agent prompt`.
- **Numbered menus want the digit, not the label.** A Claude approval/choice (`1. Yes / 2. No`)
  only accepts the option's number; sending the literal label ("Yes") is silently ignored — it
  reads as "nothing happened" on confirm. Map the chosen option to its digit with
  `domain.MenuKeystroke` before delivering (both the daemon `act` and frontend confirm paths do).
- **A label that maps to NO option must never be delivered — the literal fall-through commits
  option 1.** Verified live (2026-07-31, Claude Code 2.1.220): typing an unmatched reply at a
  standing Bash approval runs the command under plain "Yes" and reports success — the agent
  ignores the letters and the trailing Enter commits whatever option the caret rests on, which is
  always the first. So "no digit could be mapped" is not a safe default on a menu:
  `domain.UnmatchedMenuReply` is the gate, and **all FOUR send paths** refuse on it — `daemon.act`,
  the LLM promotion in `handleLLMOutcome`, the rewritten reply in `handleActionReviewOutcome`, and
  `deliver.Deliver` for operator-confirm/auto-accept. Two things make a correct label fail to map,
  and both are load-bearing: **typography** — the same build renders `Yes, and don’t ask again for:
  npm *` with U+2019 while every rule, LLM answer and fixture in this repo writes the ASCII
  `don't`, so all label comparisons go through `domain.FoldMenuText` (punctuation, case,
  whitespace); and **drift** — a rule learned on one render of an option (`use auto mode` vs
  `switch to auto mode`, a path that has since changed) names an option no longer offered, which is
  exactly what must escalate. Three ordering rules are deliberate and easy to undo by accident:
  the gate runs AFTER the multi-tab answer-series and remote-environment branches on every path
  (each answers its own protocol); it runs AFTER `llm.enable_rewrite_action` dispatches in
  `act`, because adapting a drifted label to the live options is exactly what the rewrite is for —
  `handleActionReviewOutcome` re-checks the result, so nothing skips the gate by going that way;
  and matching is unique-or-refuse on BOTH the exact and the prefix pass, since one capture can
  hold two renders of a menu that number the same label differently. Keep the paired tests
  (`TestMenuKeystrokeFoldsTypographicPunctuation` / `…FoldKeepsDistinctOptionsDistinct` /
  `…DuplicateRendersRefuse` / `TestUnmatchedMenuReply` / `TestDeliverUnmatchedMenuReplyRefuses` /
  `TestDeliverUnreadablePaneWithMenuEvidenceRefuses` / `TestAutoActMatchesLabelAcrossTypography` /
  `TestAutoActUnmatchedMenuReplyEscalatesInsteadOfSending` /
  `TestLLMPromotionUnmatchedMenuReplyRejects`). Two accepted trade-offs: an approval whose real
  prompt is a bare `y/n` while unrelated numbered lines sit in the scrollback now escalates instead
  of typing `y`; and an UNREADABLE pane refuses only when the decision's own capture proves a menu
  was standing (`req.PaneExcerpt`) — with no such evidence the literal send still stands, so legacy
  rows that carry no excerpt behave as before.
- **A digit does NOT always commit — AskUserQuestion has two protocols, per tab.** Verified live
  (2026-07-16): on **plain** options (`1. Apple / 2. Banana`) the digit selects AND auto-advances,
  but on **preview** options (option list left, `┌──┐` preview box right, `Notes: press n to add
  notes`) the digit only **moves the caret** like ↑/↓ — **Enter** commits and advances. The footer
  is identical in both and never mentions digits, and one form mixes them (a preview form's
  generated Submit tab renders plain). Blind digit-only delivery is a silent no-op on preview
  forms: nothing is answered and the agent stays blocked. Never plan a whole keystroke series up
  front — `internal/mcqdeliver` presses the digit, re-reads, and only presses Enter if the answer
  did not commit (and refuses if the caret never reached the chosen option).
- **Claude's "Select remote environment" picker (remote sub-agent launch) reports IDLE, not
  blocked.** Herdr shows no blocked status while the modal stands (verified live 2026-07-17), so
  hap detects it structurally (`domain.ClaudeRemoteEnvForm`: title + `❯ N.` options + end-anchored
  "Enter to select · Esc to cancel" footer) and classifies it as a parked APPROVAL at idle/done —
  same exception pattern as Codex's Plan approval. Verified live (2026-07-17): despite the
  "Enter to select" footer, the digit alone COMMITS the selection (the picker closes, no Enter) —
  but all paths still answer it adaptively via `mcqdeliver.ClaudeRemoteEnv` (digit → verify
  caret → Enter only if still standing) in case a build ships the caret binding, failing closed
  when the learned label matches none of the offered environments.
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
| `internal/domain/agentmode.go` | parses an agent's permission mode out of its composer footer; proves the composer is safe to press into |
| `internal/llm` | operator LLM CLI adapter (argv template, auto-repair) |
| `internal/mcpserver` | stdio MCP server (`get_context`, `submit_decision`) |
| `internal/herdr` | herdr CLI + events-socket adapters |
| `internal/store` | SQLite persistence (WAL; `context_json` is an opaque blob) |
| `internal/taskfile` | advisory file lock behind every checklist read-modify-write |
| `internal/tasklocator` | the ONE canonicalizer for a task-list locator + provider resolution (pure) |
| `internal/taskstore` | task-list backends: `local` (default) and `gist` (opt-in, the only GitHub SDK importer) |
| `internal/selfpath` | resolves a live `hap` binary (an upgrade unlinks the running one) |
| `internal/tuisession` | flock registry of live `hap tui` processes; closes the oldest past `[tui] max_instances` |
| `internal/updatecheck` | GitHub release check — one of exactly TWO `net/http` importers (NFR-007 allowlist) |
| `internal/fakeherdr`, `e2e_harness/` | test fakes and the e2e driver |
| `docs/architect/herd-auto-prompter-architecture.md` | consolidated architecture doc (FR-xxx / NFR-xxx ids used in comments) |
