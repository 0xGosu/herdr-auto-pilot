# Changelog

Every change gets a line here, including patch releases — see the Changelog
section in `CLAUDE.md`. Entries sit under **Unreleased** until the release that
carries them is tagged, then that heading becomes the version number.

## Unreleased

- Added a herdr desktop notification when a new escalation appears while the TUI is open, or when automation is paused by another process — the TUI detects that herdr launched it and raises the toast over herdr's socket API, so an escalation reaches you from another tab or another app instead of only beeping the pane you are not looking at
- The terminal bell is now the fallback rather than the only channel: it rings when there is no herdr to talk to, and also when herdr answers that it did NOT display the toast (notifications turned off, rate limited, no foreground window, or a toast already standing) — an undelivered toast never counts as having alerted you
- Added `tui.herdr_notification` (default on) to turn the toast off independently of `tui.terminal_bell`; with both off the TUI is silent
- Outside herdr nothing changes: no socket is opened and the bell behaves exactly as before
- Fixed the builtin-rule attribution behind `b` and the `hap escalations` hint being spoofable by agent-influenced text: it searched the whole rationale for a shipped rule's `(source=seed …)` marker, so a fabricated diagnostic in a pane excerpt or in appended LLM/error text could claim a rule that never fired — and because the search ran in seed-list order, a forgery naming an earlier rule could even outrank the genuine hit. You would be offered "disable this builtin rule" for an unrelated safety control while the rule that actually blocked you kept blocking. Attribution is now bound to the first diagnostic in the rationale, which is always the genuine one

## 0.5.7

- Added `b` on the Escalations tab (and in an escalation's `v` detail): disables the one builtin never-auto rule that forced the selected escalation, after a `[y/N]` confirmation naming the rule, its id, and the escalation it blocked — previously this meant leaving the TUI to match a regex by eye in `hap rules list` and run `hap rules disable-seed <id>`
- Added a `Builtin rule` line to the escalation and audit detail views, showing the rule's stable id so it can be acted on from either surface
- `b` disables only the rule that forced that escalation; it never sets the wholesale `safety.disable_never_auto_seed_patterns` switch, never resolves to a builtin when your own `never_auto_patterns` entry has the same text as a shipped rule, and does nothing on the read-only Audit tab. Undo with `hap rules enable-seed <id>`
- Fixed a rule merely *named* in a rationale counting as its cause: a `variance_guard` escalation quotes the suspected-irreversible diagnostic without having been blocked by it, so `b` is not offered there and the detail line marks the rule `noted, not what forced this`. The same gate now guards the `hap escalations` disable-seed hint, which had the identical gap

## 0.5.6

- Fixed a submodule gitlink replaced by a symlink taking down every CI job with a linker error deep inside `scripts/setup-native.sh` that named neither git nor submodules; `scripts/check-submodule-gitlink.sh` now runs first in every job that builds native dependencies, and as `make check-submodules`
- The guard checks the ref's tree and the index — the index leg is what a local `make check` catches before the bad commit exists — and rejects a symlink, contents committed as ordinary files, a missing entry, a `160000` entry naming a blob instead of a commit, and a gitlink whose `.gitmodules` mapping was deleted, printing the recovery steps
- Contributor-facing only; no runtime behavior changed

## 0.5.5

- Added `[escalations.auto_accept]`: escalations that have waited past a configured threshold can be delivered automatically instead of sitting forever. Default OFF, and fail-closed — an escalation is only eligible if it has a persisted signature baseline, and every unknown condition resolves to ineligible
- An auto-accept is a distinct claim status (`auto-accepting` while in flight, `auto-sent` once delivered) and is deliberately never rendered as `resolved`: nothing is learned from it, so it must not read like an operator decision
- Four escalation reasons can never be auto-accepted, excluded in code and not exposed to configuration: `never_auto_match` and `suspected_irreversible` (the hard-safety verdicts), plus `retry_exhausted` and `rate_limited` — auto-accepting a ceiling verdict re-sends the very thing the ceiling exists to stop, and because an auto-accept writes no correction the counter never advances, so it would loop forever unattended
- A `@noop` decision is never delivered by the pass
- Refactored the reply-delivery pipeline into `internal/deliver` so the daemon and the frontend share one fail-closed implementation (pane re-read, multi-tab answer series, Claude's remote-environment picker, menu-digit mapping) instead of drifting apart across the write partition. Operator-visible refusal strings are byte-identical

## 0.5.4

- Fixed `hap help task-source` and the runtime `task-source set` usage advertising `enable-llm-review`, a key `set` refuses — it was renamed to `enable-llm-review-before-auto-send` in 0.5.2. The runtime copy was appended to the rename error itself, so hap named the correct spelling and then printed usage with the wrong one
- Fixed `task-source add --enable-llm-review-before-auto-send` still announcing "a decline is escalated to you", which stopped being true in 0.5.2; both success messages are now shared constants so they cannot drift apart again
- Fixed `hap help tui` listing the wrong tabs and claiming complete TUI/CLI parity — the symlink shortcut and the stderr viewer have no CLI verb
- Fixed `--template` help omitting the `{cwd}` placeholder

## 0.5.3

- Added the running version to the TUI header, with a newer-release hint: `Herd Auto Prompter v0.5.1 ↑ v0.5.2 available`. Both are dropped rather than wrapped on a narrow pane (hint first), because a wrapped header would push the body past the bottom
- The release check is the plugin's only outbound network call: at most every 6h, TUI only, result cached to `<state>/update.check.json`, failures cached like successes so an offline host backs off. A `dev` build never claims an upgrade, so a linked working tree does not nag its developer
- Added `tui.disable_check_for_update` to turn it off

## 0.5.2

- **Breaking.** Rebuilt the LLM task review as a pre-**delivery** filter (`internal/daemon/tasklistreview.go`). It used to fork before `domain.Decide`, which preempted the decision — a signature graduated to autonomous on `@next_task:declared` could never act — and its only failure mode was an escalation, which bars an agent from the idle poll forever, so a reviewed auto-send source silently switched itself off
- `Decide` now decides *that* a task goes; the review decides *which* task and in what shape, and it never escalates. The mutual exclusion with `enable_auto_send_task_when_idle` is removed — the two compose
- The LLM answers in one `submit_decision`: `task_actions`, an ordered series of checklist edits (done/delete/edit/move/add), plus `send_task`, the *reference* of the task to deliver. `send_task` is an id, never text — the daemon renders the prompt from the list itself, which removes the paraphrase-drift failure mode of the old text-based design
- Edits, re-resolution and the `[-]` reservation share one locked read-modify-write (`taskfile.ApplyReview`)
- The config key `enable_llm_review` is renamed `enable_llm_review_before_auto_send`

## 0.5.1

- Added detection for two more blocking Claude API banners: `api-server-error` ("API Error: Server error mid-response…") and `api-overloaded` ("API Error: NNN Overloaded…", generalized beyond code 529)

Releases 0.5.0 and earlier are documented on the
[Releases page](https://github.com/0xGosu/herdr-auto-pilot/releases) only.
