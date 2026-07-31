# Changelog

Every change gets a line here, including patch releases — see the Changelog
section in `CLAUDE.md`.

**This file is assembled, not edited.** Contributors add a fragment in
`changelog.d/` (one file per PR, so two PRs can never conflict); the release
automation folds those into a new section here under the version it actually
assigns. Do not add a heading or an entry by hand.

## 0.5.19

- Changed BM25 text matching to also run when embedding search finds no learned rule above `similarity_threshold`, not only when the embedder is unavailable or errored — a screen that is a textual near-duplicate of a rule hap already learned now reuses that rule instead of minting a new signature, re-escalating, and graduating from scratch.
- Changed approval, choice and error rules so a screen that embedding search has already judged too dissimilar is never reconsidered by text matching. Text scoring compares words without knowing which word carries the meaning, so an approval that swaps its target (`… to the test service` → `… live service`) is indistinguishable from one whose wording merely changed — and it must not inherit the other's learned answer. This matches the rule already applied when checking whether a screen held still before a delayed reply.
- Added `embedding.bm25_highbar_score` (default 0.70), the stricter text-matching bar for screens at or above `min_salient_chars` once embedding search has run and refused them. Shorter screens, for which text matching is the only matcher, keep `bm25_min_score` and are unaffected.
- Added a stall guard to the text-matching search so a pathologically slow match index degrades to exact-hash matching instead of holding up the daemon's monitoring loop.
- Fixed a transient vector-search failure persisting the new signature without the embedding that had been computed for it, leaving that rule unreachable by similarity matching until a later daemon restart re-embedded it.
- Fixed a newly learned signature being labelled with whichever embedding model was loaded when it was saved rather than the one that produced its vector, which could permanently attach a mismatched vector to a rule if the model was reloaded at that moment.

## 0.5.18

- Fixed unrelated situations being auto-answered by one almost-empty learned rule, reported as `matched by \`similarity_threshold\` (cosine 0.91)`. Sentence embeddings are not discriminative on a handful of generic tokens, so any near-empty rule sat above the threshold from nearly every screen and became a magnet that answered them all
- Added `embedding.min_salient_chars` (default 100, measured on the masked salient): below this length a situation is matched by BM25 text search instead of embedding. The floor applies to BOTH sides — a short situation is never embedded, a newly learned short rule is stored without a vector, and an existing short rule is dropped from vector search — so such a rule stays reachable by text matching and exact hash only, never by similarity. Set it lower to restore the old behavior
- Unchanged: approval, choice and error rules still match by similarity at any length. The floor applies only to rules keyed on raw screen text, which is where a near-empty rule can be mistaken for anything; a rule keyed on a permission verb and its options is a distilled identity and is exempt
- Changed: existing near-empty rules heal themselves. The first daemon start after this release strips their vectors, so no reset or re-learning is needed to stop them firing
- Added redaction of Claude's own TUI furniture from the matched content: the startup banner, the `───` rules, the live `✽ Thinking… (12s · ↑1.2k tokens · esc to interrupt)` line, the `-- INSERT -- ⏵⏵ accept edits on` mode line, herdr's status bar, and the trailing `❯` composer line. That chrome is identical on every Claude pane, so it both made different screens look alike and crowded out the agent's actual output — a pane whose real content sat above a long footer could be matched almost entirely on furniture
- Changed: a Claude pane that is nothing but chrome now escalates as over-masked instead of learning a degenerate rule
- Note: Claude `idle`/`unclassifiable` rules re-key once and re-learn, the same one-off cost as changing `pane_salient_chars`. Approval, choice and error rules are unaffected
- Note: with the furniture gone, a Claude pane carrying only a word or two of real output can fall under the existing over-masking floor and escalate as unidentifiable. It used to clear that floor on the strength of the chrome alone — which is the same bug — so one ordinary sentence of agent output is now what makes a screen identifiable

## 0.5.17

- Changed how changelog entries are written: add a fragment in `changelog.d/` instead of editing `CHANGELOG.md`. One file per PR means two open PRs can no longer conflict on the same lines, which they did on every parallel change
- Removed the need to guess a version number. Contributors write no version at all — the release automation folds the fragments into `CHANGELOG.md` under the version it actually assigns, which is the only moment that number is a fact
- Added a CI check that fails a PR changing releasing code without a fragment, so the mandatory-changelog rule is enforced rather than remembered
- Added a release guard that refuses to tag while unassembled fragments remain, which catches the manual minor/major path forgetting to run `scripts/assemble-changelog.sh`

## 0.5.16

- Fixed `scripts/setup-native.sh` dropping `sudo` when only part of the install prefix was writable, which failed the build with `Permission denied` on `apt-get` and on `/usr/local/include/faiss`. It now keeps `sudo` unless BOTH `lib` and `include` are writable, and always uses it for the package manager, which needs real root regardless
- Changed CI to check formatting in its own job, so a `gofmt` slip is reported in seconds rather than behind a two-minute native build

## 0.5.15

- Added an instance limit for the TUI: starting `hap tui` now closes the older ones, so only the newest stays open. Every instance re-reads the whole state on a 2s tick and shells out to herdr for each agent's pane, so panes left open in other tabs kept a core busy for a view nobody was reading
- Added `[tui] max_instances` (default 1) to raise that cap — `hap config set tui.max_instances 2` keeps two; `0` restores the old unlimited behavior. It applies without a restart: lowering it closes the surplus within 10s
- An older TUI is closed the same way closing its pane is, so it restores the terminal and shuts its database down cleanly — and it is given a full minute to finish doing so before it is ever asked again. The instance that closed it says which pids it asked to close and why. A TUI whose peers cannot be read or signalled is left running: the limit is a performance guard, never a reason for a TUI to fail

## 0.5.14

- Changed the daemon to raise its herdr notifications over the socket API instead of the `herdr notification show` CLI, so it now learns whether a toast was actually displayed. The CLI exits 0 even when herdr paints nothing, which meant an escalation could be dropped — notifications turned off, rate limited, no foreground client — with the daemon assuming the operator had been told
- Added the delivery outcome to the daemon's `escalated` log line: `notified=true`, or `notified=false` with the reason herdr gave. Absent when nothing reported it, so the log never turns "we don't know" into a claim
- Kept the CLI as the fallback for when the socket itself is unreachable. A request herdr answered and refused is never re-fired through it — same herdr, same verdict — and neither is one already refused locally or cancelled by shutdown
- Changed the notification timeout to bound the whole socket-then-CLI attempt rather than each hop, so adding the socket cannot make a wedged herdr stall the daemon longer than the CLI alone did

## 0.5.13

- Fixed long text being invisible past the right edge of the TUI while typing it. Every text input — add task, edit a task, correct a suggestion, the `/` filter, every config value — now wraps to the pane width, so the whole entry is readable without breaking the sentence with `shift+enter` to see it
- Changed: wrapping breaks after a word rather than mid-word. A short entry still sits on its label's line as before; a label too long to share that line, or a box that has scrolled, gives the text the full pane width instead
- Added a scrolling input box: the box takes only the rows the pane can spare (at most 8, and never the last few list rows), and an entry taller than that scrolls with the caret and says which rows are showing — instead of pushing the list and the help line off the bottom
- Unchanged: what gets submitted. Wrapping is a rendering decision only — no line break enters the stored task, filter or config value

## 0.5.12

- Fixed `enable_auto_send_task_when_idle` still not delivering anything unattended: the task went out only once its situation signature had graduated, which took two operator confirmations — the exact human attention the flag exists to remove. Because every idle screen mints its own signature, in practice it escalated `shadow_mode` with the task as a suggestion and waited, forever
- Changed: a declared task from a source with `enable_auto_send_task_when_idle` is now delivered without waiting for the signature to graduate, and without being held to the idle confidence threshold. Turning the flag on now means what it says — the agent keeps itself fed while you are away
- Changed: a learned "do nothing" rule no longer parks pending work on such a source, whatever its provenance. The opt-in is an instruction about a queue and outranks an inference about a screen
- Unchanged for every other source: without the flag a source is attended by definition, so a shadow signature still suggests rather than acts and a learned noop still escalates over pending work
- Unchanged for safety: the kill switch, the variance guard, the per-minute and consecutive rate ceilings, the suspected-irreversible heuristic, the never-auto patterns and per-agent disable all still stop an unattended hand-out. This skips graduation, not safety

## 0.5.11

- Fixed `enable_auto_send_task_when_idle` going permanently silent on an agent that had any escalation waiting: a pending task is itself what raises `noop_vs_pending_tasks`, and that escalation then blocked the very poll that would have delivered the task, so the agent sat idle beside its own list with nothing logged
- Changed: a pending escalation no longer withholds queued work from an agent at all. It is a question about what to answer on the agent's screen, not a judgement that the agent cannot take its next task — so hand-outs continue while you catch up, and answering or dismissing an escalation is no longer a prerequisite for auto-send to resume
- Changed the limit that stops an undeliverable task from being retried forever: it now counts deliveries that herdr refused, and applies per TASK rather than benching the agent. An item whose delivery fails three times is left `[-]` and escalated as `task_never_started`, and the agent moves on to the next item instead of stalling
- Added a widening interval (1, 2, 4 … up to 15 minutes) before the idle poll re-reads the pane of an agent whose episodes keep resolving to something other than a send, so an agent parked behind an unanswered question no longer costs a pane read and an audit row every minute indefinitely. Any delivered task resets it, so nothing is ever prevented — only delayed

## 0.5.10

- Fixed the changelog leaving the 0.5.8 and 0.5.9 entries stranded under `Unreleased` after both releases shipped; they now sit under their own version headings, and the file no longer uses an `Unreleased` heading at all
- Fixed `hap` being killed outright by SIGHUP — the signal raised whenever the terminal hosting it goes away, so closing a herdr pane or dropping an ssh session while the TUI was open ended it mid-flight, with the store never closed and the terminal left in raw mode with the alt screen still on. SIGHUP now cancels the run context like SIGINT and SIGTERM, and the TUI unwinds through it. A second signal still terminates immediately, so a process that ignores the cancellation can never become unkillable
- Changed the TUI Config tab to hide ten advanced fields that crowded out the settings people actually change: `llm.pane_excerpt_chars`, `llm.enable_rewrite_action`, `llm.rewrite_action_fallback_template`, the five `llm.*env_file` paths, `embedding.pane_salient_chars`, and `embedding.warm_timeout_ms`. They are unchanged everywhere else — still listed by `hap config fields`, still settable with `hap config set`, still read from `config.toml`

## 0.5.9

- Added a herdr desktop notification when a new escalation appears while the TUI is open, or when automation is paused by another process — the TUI detects that herdr launched it and raises the toast over herdr's socket API, so an escalation reaches you from another tab or another app instead of only beeping the pane you are not looking at
- The terminal bell is now the fallback rather than the only channel: it rings when there is no herdr to talk to, and also when herdr answers that it did NOT display the toast (notifications turned off, rate limited, no foreground window, or a toast already standing) — an undelivered toast never counts as having alerted you
- Added `tui.herdr_notification` (default on) to turn the toast off independently of `tui.terminal_bell`; with both off the TUI is silent
- Outside herdr nothing changes: no socket is opened and the bell behaves exactly as before

## 0.5.8

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
