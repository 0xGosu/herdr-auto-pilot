# Changelog

Every change gets a line here, including patch releases — see the Changelog
section in `CLAUDE.md`.

**This file is assembled, not edited.** Contributors add a fragment in
`changelog.d/` (one file per PR, so two PRs can never conflict); the release
automation folds those into a new section here under the version it actually
assigns. Do not add a heading or an entry by hand.

## 0.9.29

- Fixed daemon tests failing on a loaded machine for timing rather than behaviour: two of them read state the daemon writes a moment later, and every shared test wait can now be stretched by one multiplier that CI sets, instead of each deadline being tuned by hand

## 0.9.28

- Standardized and tightened doc comments on exported symbols across `domain`, `frontend`, and `store` to follow Go conventions and maintain precision.

## 0.9.27

- Fixed hap answering an agy approval with a scope-widening option. The LLM was choosing "Yes, and always allow in this conversation for commands that start with X" at confidence 98-99 on nearly every approval, which pre-authorises a whole command prefix — every later command matching it then runs with no prompt, so it never reaches the classifier, the never-auto screen or the operator. hap now substitutes the narrowest option that still grants the request, and the audit rationale says when it did.
- Added guidance to the consult context telling the LLM to prefer the narrowest option on an approval, so the deterministic substitution is a backstop rather than the only defence.

## 0.9.26

- Fixed a task hand-out landing in an agy agent's approval menu: the empty-composer proof is now re-taken immediately before the keystrokes go out, closing the window the audit write and the checklist reservation opened. A menu choice commits on the bare digit, so a hand-out arriving there could select an option on its own.
- Fixed a task being recorded as handed out when agy had only queued it in its composer; the item is now left in progress and escalated instead of being returned to the list, which would have sent it to a second agent while the queued copy was still on its way.
- Fixed a task hand-out agy had accepted being recorded as queued, and so parked in progress for an operator: the composer is now read back only after agy has had time to repaint, off the monitor loop.
- Fixed a long task hand-out queued in agy's composer being recorded as delivered: the read-back now scales its search to the text that was sent, and a composer it cannot read back is treated as still holding the hand-out rather than as empty.
- Fixed an operator's own first keystrokes after a hand-out being mistaken for hap's text, and a hand-out wrapped mid-word not being recognized as its own.

## 0.9.25

- Added never-auto rules that match the ANSWER hap is about to send rather than the screen it is answering (`[[safety.never_auto_actions]]`, `hap config rules add --action` / `remove-action`, agent-type scoping as usual). The existing rules can only match situation text, so an option that is printed on every prompt — agy's "(Persist to settings.json)" — could not be refused without escalating every prompt and stalling the agent.
- Added shipped action rules that refuse the scope-widening menu options ("and always allow…", "Persist to settings.json", "allow all"), which pre-authorise a whole command prefix so later commands never raise a prompt at all. They are OFF unless `safety.enable_never_auto_action_seeds = true`, because refusing an option only escalates it — hap has to prefer the narrowest option first, or most approvals would become escalations.

## 0.9.24

- Fixed the escalation queue filling with the same "send next declared task" proposal: a hand-out is no longer proposed while the list already has an item in progress, and a repeat within one parked spell is folded into the row the operator already has instead of adding another

## 0.9.23

- Fixed a daemon crash under concurrent fleet sync. Waiting a bounded time for in-flight Turso sync operations stranded a waiter on a `sync.WaitGroup`, and the next operation to start as the counter fell to zero panicked the whole process with "WaitGroup is reused before previous Wait has returned" — twice in one night on a machine driving two agents, each crash followed by a restart that raced the dying daemon's lock.
- Fixed a Turso sync operation being able to start while the database was closing, when it would have run against a handle already being freed; it is now refused and reported instead.

## 0.9.22

- Removed unreferenced internal helpers (`NodeWatching`, `SeedRuleDisabled`, `TaskFilePath`, `GetTask`) across domain and frontend layers.
- Consolidated duplicate signature truncation logic across the CLI and TUI into `domain.ShortSignature`.
- Consolidated duplicate CLI row formatting helpers.

## 0.9.21

- Added real-agy integration cases, gated on `HAP_ITEST_AGY=1` (detection, a shell approval and a question answered through `hap confirm --send`, a task hand-out refused over a draft and delivered at an empty composer, and the permission-mode cycle), so agy's live screens and keys are checked the way claude's and codex's are
- Fixed the real-agent mode integration cases, which were refused by the front end's roster check because their test app had no daemon behind it

## 0.9.20

- `hap mode` and the `hap agents` MODE column now cover agy: its `default → acceptEdits → plan` cycle is read from agy's status bar and set with shift+tab (agy's own `accept-edits` spelling works too); hap presses into agy only at an empty composer, and a status bar with no model shows `-` rather than a guessed `default`
- An agy launched with `--dangerously-skip-permissions` shows no mode indicator, so it reports whatever its cycle shows (`default` at launch); `hap help mode` and the README say so
- An agy LLM command run with `--output-format json` now has its conversation id recorded on the audit row, like codex's session id
- `hap skill install agy` (and the TUI's skill shortcut) installs the hap skill into agy's global skills directory, `~/.gemini/antigravity-cli/skills`

## 0.9.19

- hap now answers agy's prompts itself — learned rules, LLM answers, auto-accept and `hap confirm/resolve --send` alike: a numbered approval or question gets the option's digit with no Enter, the trust-folder prompt gets its caret walked to the chosen row and confirmed, and the plan-artifact review panel gets `y`/`n`; every key is checked against a fresh read of the pane, and hap stops rather than press a key twice
- A multi-question agy form is followed through question by question: after each answer hap looks at the pane again, so the next question is decided (or escalated to you) instead of stalling unseen
- A reply meant for one agy question is never typed into the next one, even when the two offer the same labels, and a question's free-text "Write-in..." row is left to you
- Task hand-outs, generated-task sends and free-text replies to agy wait until agy shows an empty composer, so a task can no longer land in a standing agy prompt, a picker or your half-typed draft

## 0.9.18

- Added Antigravity CLI (`agy`) as a recognized agent: its approvals (shell commands, file access/creation, the trust-folder prompt, plan review), questions and errors now escalate as what they are instead of reading as an idle agent — which also stops an idle task hand-out from being typed into a standing agy prompt
- agy's banner, model line and status bar are left out of its screen signatures, so a model or effort switch no longer splits one situation into several rules
- hap does not send replies to agy prompts yet (they escalate as `reply_withheld`; answer them in the pane), and sign-in, terms, pickers and panels escalate as unclassifiable rather than being answered

## 0.9.17

- Added a recorded corpus of Antigravity CLI (agy) screens and a design for supporting it; hap's handling of agy is unchanged for now (every agy screen still reads as idle)

## 0.9.16

- Added `HAP_ACTOR=orchestrator`, which marks everything a hap command does as the orchestrator's rather than the operator's — for an orchestrating agent hap did not start itself. It gets the same never-auto screening and pause refusal as commands run in the orchestrator's own pane. That pane is always the orchestrator whatever the variable says, and any value other than `orchestrator` or `operator` fails the command
- The audit log now records who settled each escalation: the TUI Audit tab has a BY column (`op`, `orch`, or `-` for hap's own rows and older ones), its detail view shows `Settled by`, and `hap audit` rows end with `by=`. Confirms, resolves, dismissals and prunes are all attributed, including ones the daemon carries out on the orchestrator's behalf

## 0.9.15

- `hap status` and `hap agents` no longer hash the whole ~25 MB embedding model on every run: the model's id is remembered per machine and re-checked only when the file changes (tens of milliseconds on an idle machine, much more on a busy one)
- The CLI verbs no longer ask the daemon for per-agent lifetime counters they never print (two full audit-log aggregations per `hap status`)
- `hap agents` reads its agents' permission modes in parallel instead of one herdr call after another

## 0.9.14

- Cut the daemon's memory: its startup peak no longer climbs to ~130MB while the semantic index is rebuilt (the burst is collected tightly and handed back to the OS at once), and each of the Turso engine's pooled connections keeps a far smaller page cache
- The embed worker hands back its start-up garbage once the model has loaded
- `HAP_PROFILE_DIR` now also writes a `<verb>-<pid>.mem.txt` beside each heap profile, splitting the Go runtime's memory from the process's resident set
- Cut the load the daemon puts on the herdr server: it now subscribes to agent-status events only for panes that host an agent, instead of every pane — with 13 panes open that was more than half of herdr's CPU

## 0.9.13

- Cut what an open `hap tui` costs: it now re-reads its data only when something changed (a store change token, the config file, local checklist files) instead of every two seconds, and the Rules tab's last-used lookup no longer pulls every rule's full audit row
- The daemon's roster tick backs off (up to 15s) while the herd's listing is unchanged, and returns to 2s on any change or agent transition
- A roster publish or read-only transaction that changed nothing no longer triggers a Turso push

## 0.9.12

- Fixed the LLM re-rank judge's in-flight answers being thrown away on nearly every Turso pull: a pull now retires them only when it actually changed the rules, their learned state or their decision history, so under turso the judge's subprocess no longer mostly runs for nothing
- Reduced the idle daemon's CPU further: the per-pull check for changed rules no longer reads every stored vector, and with no local TUI open the roster tick asks the store whether a remote TUI is watching at most once per 15s instead of every 2s
- Added an opt-in profiling hook: set `HAP_PROFILE_DIR` (and optionally `HAP_PROFILE_SECONDS`, default 60) and any hap process writes rolling CPU and heap profiles there for `go tool pprof`; nothing is written or listened on when it is unset

## 0.9.11

- Fixed the daemon rebuilding its whole semantic match index after every Turso pull, even when no learned rule had changed — on a fleet node that kept an idle daemon at ~20% CPU; it now rebuilds only when a rule is added, removed or re-embedded
- Reduced `hap stream orchestrator`'s idle cost: a caught-up stream no longer re-reads the log's retained floor on every poll, and it polls every 2s instead of every 500ms after 30s without events (the first event restores the fast poll)

## 0.9.10

- Added `full_self_prompting.orchestrator_agent_command` (bootstrap it with `hap config set full_self_prompting.orchestrator_agent_command --preset claude`): while full self-prompting is on, the daemon keeps an interactive claude session named `orchestrator` alive in its own `hap-orchestrator` herdr workspace, briefs it to watch `hap stream orchestrator` and keep the herd moving toward the goals you type into it, and ignores it completely. It is re-created if it disappears (at most 3 times an hour) and never closed by hap; `full_self_prompting.orchestrator_agent_prompt` replaces the built-in brief.
- The orchestrator's row is highlighted on the TUI Agents tab.
- Added `full_self_prompting.orchestrator_agent_cwd` to start the orchestrator in a directory of your choosing (it must already exist) instead of `<state>/orchestrator`.
- The orchestrator's built-in brief schedules an hourly health check that restarts a stopped hap daemon and looks in on hung agents, and removes it while full self-prompting is off.
- A failure to start the orchestrator (or a first-run claude prompt holding its brief) now shows as a TUI banner and in `hap status`, and the Config tab warns when `orchestrator_agent_cwd` does not exist.
- Added `hap stream orchestrator [--resume N]`, a live one-line-per-event stream of what an orchestrating agent reacts to: config and task-source changes, task items, database task lists, escalations that auto-accept left for a human, dismissals and corrections, pause/resume, full self-prompting on/off, manual rule edits, and daemon restarts. Every line carries a resumable sequence number; events are kept for 7 days in a machine-local log.

## 0.9.9

- Added `hap signatures search --screen`, which searches the captured pane instead of the rule's masked salient — the literal paths, commands and version numbers a salient replaces with `<path>`/`<num>` placeholders are findable again. It matches every term anywhere in the screen (quote a phrase to require it contiguous), reports how many screens it searched so an empty result tells you whether there was anything to search, and each result carries the text around its first hit.
- Added ctrl+g on the TUI Rules tab, running that same screen search over the captured panes.
- Changed `--limit` to bound a `--screen` search as well as a `--semantic` one; keyword search stays unbounded.
- Fixed the `match=` excerpt on a screen search drifting away from the term it found when the screen contained characters that change byte width when lower-cased.
- Fixed the suggested follow-up commands re-emitting a quoted search phrase as separate words, so pasting one searched for something wider than the original query.

## 0.9.8

- Fixed the confirm-delivery integration tests, which could not pass since 0.8.0 and had silently switched off the guard on the send-content regression (that confirming a label reply selects the numbered menu digit rather than pasting the label).
- Fixed the task-summary integration tests, which escalated instead of consulting because their scratch pane's output was short enough to fit on screen, and the daemon's classification read returns nothing until a pane has scrolled.

## 0.9.7

- Changed the Claude session-name sync to leave you alone: it acts on a session only while the agent is parked, its composer untouched, and it has been sitting quietly — so a `/rename` no longer lands on a session you have just opened and are about to type into. In practice a name now settles a minute or two after an agent goes quiet, rather than seconds.
- Changed those checks to run twice, the second time immediately before the keystroke and against the agent's live status rather than the status its capture carried, so an agent that went back to work in between is left alone.
- Changed a refused rename to be retried about a minute later (backing off to fifteen), instead of waiting for an attention event a quiet pane may never produce.
- Fixed a refused rename counting against the three-keystroke ceiling: being mid-draft three times used to disable the rename for that agent permanently.
- Fixed a rename still landing on an agent that went back to work and parked again while the check was in flight: the quiet-enough test now runs against the agent's current parked spell, not the one its capture saw.
- Fixed turning the setting on being silently ignored when a retry happened to be running: the enable is now remembered and the one-shot sync runs as soon as the retry finishes.

## 0.9.6

- Added a loud TUI banner and `hap status` detail for a node whose shared-database sync has stopped working. A wedged sync engine used to be invisible: the daemon kept running, the herd looked quiet, and the Escalations and Agents tabs silently showed only this machine's rows while the other nodes' were never arriving — the failure was recorded at Warn level and nowhere else. It now says what it MEANS ("this machine is NOT exchanging rows with the other nodes") rather than what failed, and only after five minutes without a successful pull or push, so a passing network blip stays a quiet warning.
- Added an automatic recovery for a sync engine that has wedged inside the process: after five consecutive failures spanning at least five minutes, and only for a fault a fresh process is known to clear, the daemon hands the herd to a new one — the same thing `hap daemon --restart` does, without waiting for someone to notice. The observed case is macOS failing every TLS handshake with `SecPolicyCreateSSL error: 0` until the daemon is restarted.
- Changed nothing about a node whose REMOTE is simply unreachable: a timeout, a DNS failure, a refused connection or a rejected token is left alone, because a restart cannot fix those and would cost the herd its in-flight captures and consults every time. An unrecognized error is treated the same way and left to the operator. A daemon born from a recovery never orders another until one pull or push has actually succeeded, so a fault a restart cannot fix can never become a restart loop.
- Added a distinct report for a node still waiting on its FIRST shared database, which used to look like an ordinary running daemon: the retry is unbounded and the daemon holds its lock throughout, so `hap status` said "running" for a process that had not begun monitoring anything. A cold start stays a quiet warning; a wait past five minutes now says the daemon is not monitoring and points at `database.turso_database_url` and the auth token.
- Added the descriptor budget at the moment of a sync failure — the limit, the open count where the platform can report one, and whether opening one more actually failed — to `hap status` and the TUI, so the next occurrence names its own cause instead of needing another investigation.

## 0.9.5

- Added `hap task <agent|--path|--node …> drop-list`, which deletes a whole checklist kept in the hap database — the list-level counterpart to `remove <n>`, which only ever deleted one task. It asks before acting (`--yes` to script it), works on another machine's list when the store is shared, and refuses a list kept in a file or a gist, which are yours to remove rather than hap's
- Added `X` on the TUI Tasks tab: the same removal from a list header, with a confirmation naming how many tasks go with it. It reaches a fleet list too, which is the only way to clear a dead list belonging to a node you are not sitting at
- Added an automatic reclaim of `sqlite`-provider checklists nothing can reach any more: on the daemon's existing daily retention pass, a list on this machine that no task source names, that no live agent owns, and that has not been written for the retention window is deleted. Until now a `task_lists` row was immortal — removing its source or retiring its agent left it in the database forever, syncing with every other change
- The reclaim rides `[logging] row_retention_days` (default 30 days, negative to switch it off) with its own 7-day floor, so setting that key to 0 or 1 to clear bookkeeping rows aggressively never reaps a checklist written this week. It skips the whole pass rather than guessing whenever it cannot tell which agents are live, so a daemon that has just started, or one whose herdr is down, deletes nothing

## 0.9.4

- Fixed the embedding model's identity: it is now a digest of the model file, not the file's name. Two different models both installed as `model.gguf` used to report the same id, so a stored vector computed by one was kept and compared against the other — silently mixing vector spaces. The same model at a different path or under a different name now also reports the SAME id, which is what stops nodes sharing one database from re-embedding each other's rules forever.
- Changed: after upgrading, the first daemon start re-embeds every stored rule once (they carry the old file-name id, and nothing recorded what those vectors were really computed with). `hap status` shows the drift until that pass finishes.

## 0.9.3

- Added `llm.reranking_command`: an optional LLM judge that re-ranks the learned rules an embedding search found. With it set, `embedding.similarity_threshold` becomes a filter rather than the decision — every rule at or above it is shown to the judge, which answers with the ones that genuinely match, and hap uses the first. An empty answer means no rule matches, so the situation is learned as new instead of inheriting a rule that only looked similar; the BM25 text fallback is skipped there, since it would otherwise re-admit the rule the judge just refused. Off by default, and a judge that fails, times out or answers unparseably falls back to the match hap would have made without it, so a broken judge never costs you a rule you already taught it.
- Added `llm.reranking_top_k` (3), `llm.relevance_score_threshold` (0.95), `llm.reranking_max_candidates` (10) and `llm.reranking_timeout_seconds` (30), plus `--preset claude` / `--preset codex` recipes and a `reranking_command` scope for `hap config env`. The re-ranking timeout deliberately does not inherit `timeout_seconds`: the agent stays parked and unanswered while the judge runs.
- Added a `hap status` line naming the re-ranking settings in force, and an escalation raised because the judge refused every candidate now reports `rerank_veto` in `hap audit` — otherwise a veto is indistinguishable from nothing having matched.
- Changed the re-ranking judge to stay silent while the herd is paused: the kill switch is checked before the judge is launched, so pausing stops it spending tokens on decisions that escalate anyway.
- Changed a re-ranking verdict that was still being computed when the config reloaded or a fleet sync brought new rules to be discarded rather than applied: it answers a question that no longer stands, and the decision falls back to the match hap would have made without the judge.
- Changed the shipped judge recipes to grant the run nothing: the claude preset now passes `--tools ""` so Claude's built-in tools are removed outright, alongside `--strict-mcp-config`, and the codex one runs read-only. The judge answers from its prompt alone but runs in the monitored agent's own project directory, so the grant is closed rather than merely unused.
- Changed the re-ranker to use the judge's whole ranked answer rather than only its top pick: the judge scores how well a rule fits the screen and cannot see whether that rule has graduated, so its best match is often one hap may not act on yet. Each rule it affirmed is now tried in order and the first that resolves on its own is used; when none can, the best match is still what you are asked about. No safety control is skipped along the way — the kill switch, never-auto patterns, the irreversible-command heuristic and the rate guard refuse every candidate or none.
- Changed `llm.reranking_max_candidates` to default to 20 (was 10), so the judge sees a deeper field and has more to fall back to, and `llm.reranking_top_k` is now refused below 1 — omit the key to get the default of 3.

## 0.9.2

- Added immediate feedback on the Escalations tab when you answer a row. The
  row dims and its first column shows `»` the instant you press the key, and
  the banner names where the answer is going ("answering escalation #41 on node
  laptop — queued for its daemon…"). Answering another machine's escalation
  files the request for that node's daemon and waits up to 45s for its verdict,
  and for that whole window the row used to look untouched — so there was
  nothing to tell "it is on its way" from "the key did not register".
- Fixed a second press on a row already being answered queueing the work twice.
  It is now refused, saying the answer is already in flight. Covers every row
  action from both the list and the detail overlay: confirm+send, confirm-only,
  correct, retry LLM and delete.

## 0.9.1

- Changed the Escalations tab's "prune old" (`X`) and `hap escalations prune` to
  retire every machine's aged escalations, not just this one's. The list they
  prune has always spanned the fleet under a shared `turso` database, so a
  node-scoped prune cleared a fraction of what was on screen and reported the
  count as though it had cleared all of it. Both surfaces now name the scope
  ("across 3 nodes"), and `hap escalations prune --node <label|id>` prunes a
  single machine.
- Fixed `f` on another machine's task list saying "no live agent matches this
  task source". It now focuses that node's agent, the way `f` on the Agents and
  Escalations tabs already did; a list the owning node never named an agent for
  says that instead.
- Fixed `K`/`J` refusing to reorder another machine's task list with "this task
  source is no longer loaded" — about a list that was on screen. Reordering
  works on any node's list.
- Fixed `x` on another machine's task-source header doing nothing at all. It now
  explains that a task source lives in that machine's `config.toml`, which never
  enters the shared database, and points at `hap config task-source remove`
  there.
- Fixed the README's fleet section still saying renaming, enabling, disabling or
  focusing a remote agent is refused — `--node` has filed all four for the
  owning daemon since the fleet view landed. Changing a remote agent's
  permission mode is the one that really is refused.
- Fixed `--node=` with no value being read as "no `--node` at all" on every verb
  that takes the flag. It was a silent no-op there, and would have been the
  wider action on the new fleet prune; it is now refused like the bare `--node`.
- Fixed the Tasks tab treating a fleet task-list read FAILURE as another
  machine's list: focus, send and source-removal named an empty node and
  pointed at a machine that does not exist. They report the store error now.

## 0.9.0

- **Breaking.** Changed the default task-list backend for NEW installs to `sqlite`: a
  checklist is a row in hap's own database instead of a markdown file, so `hap task`
  needs no file lock, each matched agent gets its own list without you naming a path,
  and under the turso engine the lists are visible fleet-wide
- An install that already has a `config.toml` keeps `local_fs` — hap pins it on load,
  before anything else reads the file — so no checklist you already have changes where
  it lives, and no source changes locator
- Changed `hap config task-source add` on a fresh install: the `<checklist.md>` argument
  is now optional (one list per agent is derived), and a filesystem path is refused with
  a message naming the two ways out — `--provider local_fs`, or omit the path
- Added `--agent`, `--workspace` and `--provider` to the TUI's add-task-source prompt,
  so the pathless per-agent form is expressible there at all: the prompt took the
  checklist as its FIRST positional field, leaving an operator on a fresh install with
  nothing to type in its place
- Changed the "add a task source" guidance printed by `hap task`, `hap confirm`, the
  help pages and the bundled skill to the pathless form, and it now shows
  `--provider local_fs` beside the filesystem examples — the commands it suggested
  failed outright on a fresh install
- Fixed `hap config task-source add --help` omitting the `sqlite` provider from its
  `--provider` values and from what `<checklist.md>` means

## 0.8.8

- Fixed the TUI Audit tab and `hap audit` labelling another machine's rows with a local
  agent's name: a herdr pane id repeats on every node sharing the store, so audit rows
  now resolve the same `name@node` identity the Escalations tab already used — and an
  audit row from another node no longer borrows a local agent's type
- Added an `agent=` column to `hap audit`, which never had one; it is appended beside
  `node=` so nothing parsing the existing tab-separated fields moves
- Added the node to an audit/escalation detail view when the row came from another machine

## 0.8.7

- Fixed `agent_roster` growing without bound: retired agents' rows are now deleted on the existing `[logging] row_retention_days` window, with a compact permanent tombstone left behind so a late transition cannot bring a dead agent back live
- Fixed a new agent on a recycled pane id being invisible until the next roster publish — the tombstone records the terminal it was retired under, so a different terminal on the same pane is admitted

## 0.8.6

- Added: confirming an LLM-suggested task now works for an agent on another machine. It used to be refused outright ("this generated-task suggestion belongs to node …"), because the confirm wrote the task list and registered the task source on whichever machine you typed on — and pointed the send at whatever local pane happened to share the agent's id. The owning node's daemon now does that work, wherever you confirm from.
- Changed: a confirmed task hand-out is recorded in the reservation ledger, so an item marked in-progress for an agent that never starts it is returned to `[ ]` automatically instead of staying stuck. While a hand-out is outstanding that agent is skipped by the idle-task poll until it starts working.
- Changed: `hap confirm` on an LLM-suggested task now needs a running daemon even without `--send`, since the daemon is what performs the confirm.
- Added: sending a task from the TUI's Tasks tab now works for a list another machine keeps, instead of refusing with "this list belongs to node …". `hap task send` stays local-only — it reads the list through this machine's task sources.
- Changed: task hand-outs and generated-task confirms are performed by the daemon, so the TUI and CLI no longer type into agent panes themselves. Two processes can no longer drive one pane, and every hand-out now passes the same idle re-check and per-agent automation lock the daemon's own sends do.

## 0.8.5

- Changed a remote focus (`f` on another node's agent in the TUI) to push the shared database immediately instead of waiting out the sync loop's write debounce, so the request reaches Turso Cloud in time for the owning node's next pull. The pull interval on that node (`database.turso_sync_interval_seconds`, default 15 s, minimum 5) remains the larger half of the wait.
- Added remote focus to the Escalations tab: pressing `f` on another node's escalation now asks that machine's herdr to jump to the pane, where it used to refuse with "focus is local-only".
- Fixed `f` in an escalation's detail overlay focusing the wrong agent: it resolved the agent id without its node, and an agent id is a herdr pane id, so a remote escalation on pane "1" moved this machine's view to its own pane "1" — a different agent, under a success banner.

## 0.8.4

- Added `hap daemon --restart`: stops the running daemon whatever binary it came from and starts a fresh one (starting one when none is running). This is the only way to pick up a `[database]` or `[logging]` change — those sections are read once, when a process opens its store, so `hap daemon --ensure` deliberately does nothing when the running daemon is already the current binary, and switching engine, pointing at a different Turso database, rotating its token or changing `node_label` previously needed finding the pid and sending SIGTERM by hand.
- `--restart` reports what actually happened rather than what it asked for: it waits for a heartbeat published after the start by the process now holding the daemon lock, and when none arrives it says the start could not be confirmed, points at `hap status --stderr`, and **exits non-zero** so `hap daemon --restart && …` never proceeds on an unknown. A `[database]` error exits *after* the new daemon has taken the lock, so a failed switch otherwise looked exactly like success — announced by the one command that had just stopped a working daemon to get there.
- `--restart` no longer feeds the crash-loop breaker. Three daemon starts within 90 seconds auto-disable semantic matching (two more stop the daemon entirely, for herdr's hook as well), and `config set` + restart, repeated while getting a setting right, is a normal minute's work — so an operator-typed restart clears the recorded boot history. Every latch is left standing: a breaker that has already given up still refuses to start, now saying so on the terminal and naming the `[embedding]` change that clears it.
- Added `hap daemon --reload`: asks the running daemon to re-read `config.toml` without restarting it — the same nudge every `hap config` write sends, now reachable on its own. It fails when no daemon is running rather than reporting success against a dead socket, and names the settings it cannot reach.
- Changed `hap daemon` to refuse `--ensure`, `--restart` and `--reload` together; each replaces the whole run, so a combination has no honest interpretation.
- Changed the remedies that pointed at `hap daemon --ensure` for cases it cannot fix — a hung daemon, and the note printed after a `hap config set database.*` — to name `--restart`, which is the command that actually acts there.

## 0.8.3

- Changed the roster publish to skip agents whose stored row already matches, so a settled herd writes one row per publish instead of one per agent. An agent's "Last transition" now reflects when it actually moved rather than the last publish.
- Changed the `nodes` heartbeat to write at most once every 30 seconds instead of once every ten. The local daemon health file keeps its faster beat, so `hap status` is as responsive as before; under the Turso engine an idle install syncs a third as often. A node now reads as stale after 90 seconds rather than 30.
- Added `[logging] row_retention_days` (default 30): the daemon now deletes FINISHED bookkeeping rows past that age — completed agent actions, resolved LLM requests and decisions, processed corrections and retries, superseded kill events and confirmed task hand-outs. Set it to 0 to keep nothing finished, or to a negative number to keep everything forever. Rows the daemon or an operator may still act on are never touched at any age, `audit_log` and `decisions` are never swept at all, and even 0 cannot delete a row a caller is still waiting on.
- Added blanking of a consult's bulky payloads (the LLM prompt context and the captured CLI output) an hour after it was raised, which is the bulk of the bytes those tables hold. The rows themselves stay until the retention window.

## 0.8.2

- Added full remote agent management to the TUI Agents tab under the turso engine: another machine's agents are now ordinary rows you can select, view, rename, enable, disable and focus — the request is queued and that machine's daemon runs it, so it lands when that node next syncs.
- Changed the Agents tab LOCATION column: a local agent still shows its herdr `#<workspace>-<tab>` position, a remote one now shows its machine's node label instead — the coordinate that actually helps you find it. The NAME column drops the `@node` suffix, since the node is in LOCATION now.
- Added `--node <label|id>` to `hap rename`, `hap enable`, `hap disable` and `hap capture`, matching `hap pause --node`. Without it an agent that exists only on another machine is still refused rather than guessed: every herdr has a pane "1".
- Added "see tasks" for a remote agent whose task source uses the `sqlite` provider — `t` jumps to that machine's list in the shared database. A node keeping its lists in files or gists is invisible from here and now says so instead of pointing at this machine's config.
- Fixed remote agent rows ignoring the Agents tab search filter, and vanishing entirely when the filter matched no local agent.
- Fixed the "stale" marker on a remote agent row being truncated away for every status longer than two characters.
- Changed `hap mode`/`hap agent mode` to refuse a target that names an agent on another machine. It sends keystrokes and resolves against the local herdr, so an agent id or name shared with another node could rotate the wrong agent's permission mode on this machine and report success. Setting a remote agent's mode stays local-only for now.
- Changed a rename to report the name the owning machine actually stored: agent names are unique per machine, so a remote rename can come back adjusted, and it now warns when that machine syncs names from Claude sessions and may re-adopt.
- Changed a queued remote action to require the owning machine to be actually looking: it must have published its agent list recently, not merely be reporting in. A daemon that is heartbeating but can no longer see herdr has a frozen view of which agent is on which pane, and herdr reuses pane ids — so acting on that view could rename, or re-enable automation on, a different agent than the one you meant.
- Fixed a queued rename or enable/disable landing on an agent that is no longer running. An agent's name record outlives the agent, so "a name exists" was never evidence the agent does.
- Known edge: without `--node`, a bare agent name still resolves against this machine first, and that can match a STALE name row for a pane that no longer exists — so `hap rename <name>` can report success about a dead local agent while the remote one you were looking at is untouched. Use `--node` (or the TUI, which always knows which machine a row is on) when a name exists on more than one machine.
- Updated the bundled `hap` skill: it documented remote rename/enable/disable/focus as refused, which is no longer true, and now covers `--node`, the queued round trip, the refusals, and what the TUI's Agents tab does for another machine's rows.

## 0.8.1

- Added a section to the bundled `hap` skill covering the `turso` database engine: how to switch to it, that the engine changes only when a process opens its store (so the daemon must be stopped, not just `--ensure`d, and open TUIs must be reopened), and what the automatic one-time import from the local SQLite database does and does not carry over.
- Changed the TUI's Escalations, Audit and pause/resume tables to show a row id as its last five digits (`…15968`) instead of the full 18-digit turso id, which used to push every later column out from under its header and clip the rationale. The `…` marks the truncation, a short id still prints whole, and `v` shows the id in full — as does every CLI listing.
- Fixed an Audit tab row with a long action wrapping onto a second terminal line: the width the tab reserved for its fixed columns was written for an 8-wide STATUS column while that column is 11, so a full row ran two cells past the pane and drew more lines than the row budget allowed, pushing the help line and the last rows off a full screen.

## 0.8.0

- Added an opt-in central database: `[database] engine = "turso"` keeps the whole store in a
  Turso sync database the daemon syncs with the Turso Cloud database you own, so several of
  your machines share one hap — every machine's agents, escalations, audit, learned rules and
  pause state in one TUI, and an escalation raised on one machine can be confirmed, answered
  or dismissed from another. The default stays `sqlite`, a local file, with no outbound call.
- Added a per-machine node id (`<state>/node-id`) to every row a machine owns, so identical
  herdr pane ids on two machines never collide, and the daemon only ever acts on its own
  agents' rows — including at startup, where one machine's restart no longer reclaims another
  machine's in-flight work.
- Changed `hap status` to report the shared database's sync state (`fleet sync:`) under the
  turso engine, and `hap config show` to print the engine when it is not the default.
- Added `database.turso_database_url`, `database.turso_auth_token` (rendered redacted; falls
  back to `TURSO_AUTH_TOKEN`), `database.turso_sync_interval_seconds` and `database.node_label`.
  The section is read when a process opens its store, so changing it needs `hap daemon --ensure`.
- Under the turso engine the TUI, the `hap` verbs and the MCP server reach the store through
  the running daemon; with no daemon they report that the store is served by the daemon
  instead of showing an empty database.
- Added a unified fleet view under the turso engine: `hap agents` and the TUI Agents tab list
  every machine's agents (remote rows carry the machine as their last field / `name@label`),
  escalations, audit and kill history carry `node=<label>`, and `hap status` lists the other
  machines and their sync freshness. Confirm, answer, correct, dismiss and retry work on
  another machine's escalation and report `queued for node <label>`; `hap pause --node` /
  `hap resume --node` pause one machine; rename, enable/disable and focus of a remote agent
  are refused because those rows belong to the owning daemon.
- Changed `hap task <agent> …` to accept either spelling of an agent — its pane id or its short
  name — for a source that derives one list per agent, and to derive that list from the NAME
  whichever was typed, so the CLI opens the same list the daemon hands out from. `hap confirm`
  and `hap resolve` say which node acts on another machine's escalation, and `hap status`
  splits the fleet's pending count by node.
- Added the `sqlite` task-source provider (`task_source_provider.provider = "sqlite"`, or per
  source): a checklist kept inside hap's own database rather than a file, addressed as
  `db://<node>/<name>`, so under the turso engine every machine's Tasks tab shows and edits
  every other machine's lists, and `hap task --node <machine> <agent> …` opens one from the
  CLI. Lists belong to one machine — only its daemon hands items out — and edits are
  compare-and-swapped on a revision, so no file lock is needed.

## 0.7.6

- Fixed the daemon losing bookkeeping writes while it published the running
  agents. Publishing took a database transaction on the loop that handles every
  agent, on the same paths as the daemon's own work — and because those
  transactions began as readers, one that committed in between made another
  fail outright with `database is locked (517)`, which is not retried. The
  daemon logs such a failure and carries on, so the write simply never
  happened: an agent's first-seen time was left unreset after its pane had been
  recycled. Publishing now runs off that loop entirely, transactions take their
  lock up front, and the database hands out enough connections that a read no
  longer waits behind an unrelated write
- Changed when the herd is republished. It happens at startup, on the
  once-a-minute sweep, and every two seconds while a TUI is open — no longer on
  every operator action. Nothing reads any fresher for it: `hap agents` and
  `hap status` are pure reads and never woke the daemon in the first place
- Fixed an agent's status briefly reverting after it started working. The daemon
  lists its agents and records that listing a moment later, so a status change
  arriving in between was overwritten by the older reading — and with no TUI
  open the next listing is a minute away. An agent that had just gone to work
  could read as free for that minute, including to `hap task send`

## 0.7.5

- Changed focusing an agent (`f` on the Agents, Escalations and Tasks tabs) to go
  through the daemon instead of the TUI driving herdr itself. It now needs a
  running daemon and reports `no healthy hap daemon is running` without one. The
  keypress no longer waits for herdr, so it now says it ASKED rather than
  claiming the pane moved, and a focus that never happens is reported in the
  daemon log. A request the daemon could not act on within two minutes — a
  daemon that died before draining it — is refused rather than replayed into
  whatever pane you had moved on to
- Changed `hap capture` the same way. It now TELLS YOU when it cannot do what
  you asked: naming an agent that does not exist, or catching one mid-work, used
  to print `capture queued` and then silently do nothing, because the request
  went out as a fire-and-forget signal with no way to answer back. Both now fail
  with the reason. It also refuses a daemon that holds its lock but has stopped
  making progress, or whose binary was replaced underneath it — cases the old
  version check reported as running
- Changed every surface that shows the herd — the TUI's agent list, `hap status`,
  `hap agents`, and anything resolving an agent by name — to read what the daemon
  publishes instead of asking herdr itself. One process now asks, however many
  windows you have open, and a working directory is read once for everyone rather
  than once per agent per window. The daemon refreshes faster while a TUI is
  actually open and otherwise only when something changes, so an install nobody
  is watching does no polling at all. One consequence is worth knowing: an agent
  that DISAPPEARS produces no event, so with no TUI open a pane that has closed
  can still be listed by `hap agents` and `hap status` for up to a minute, until
  the next sweep reconciles the herd
- Fixed a stale report being treated as live agents. When the last report is too
  old to trust, the herd now reads as UNKNOWN rather than handing every surface
  a list of agents to act on — a pane id from an old report may since have been
  recycled onto a different process. The surfaces say how old the report is
- Fixed the herd reading as EMPTY when nothing had looked at it. "No agents are
  running" and "no daemon has reported yet" are different answers, and surfaces
  that act on an agent's absence — retiring a task source, confirming an agent is
  idle — now refuse rather than guess when the report is missing or too old
- Removed the last daemon signal that carried data. A manual capture used to
  smuggle its target into the signal itself, which is why it had no way to
  report a result; signals are now purely "there is new work in the database",
  as they were always meant to be
- Added a build-time guard that stops the TUI and CLI from reaching herdr
  directly, per call site rather than per file. Each one that still does is
  listed with the reason and the migration stage that removes it, and an entry
  that is no longer needed now fails the build, so the list can only shrink

## 0.7.4

- Changed: turning `[agents] sync_claude_session_name` on now syncs every live claude agent right away instead of waiting for each one's next attention event. The sync used to be a side effect of a pane CAPTURE, and nothing re-captures on a config change — a herd already parked could sit unsynced for hours, or until every agent had gone back to work and parked again. The flip now walks the live agents once on its own, reading each pane's CURRENT screen rather than the consuming delta the classification read takes, so a quiescent agent is seen too. Every write gate is unchanged: only parked (idle/done) agents are typed into, and the kill switch, the per-agent disable, the never-auto screen and the empty-composer proof all still apply. Ordinary captures keep their existing behaviour, including the case where the delta shows no composer and the sync silently waits for the next one.

## 0.7.3

- Added `[agents] sync_claude_session_name` (off by default): keeps a claude agent's hap short name and its Claude conversation name — what `/rename` sets, painted in the composer's top rule — CHARACTER-IDENTICAL. A named session is folded into a storable short name (lowercased, non-alphanumerics to `-`, capped at 32) and, when that differs from what the session shows, `/rename <folded name>` is sent back so both sides read the same string: `My Feature: Work #2` becomes `my-feature-work-2` on the agent AND in Claude. An unnamed session is sent `/rename <hap name>` instead.
- Added a collision rule for the above: when two claude sessions carry the same conversation name, the second agent takes `name-2` and that suffixed name is pushed back to its pane, so the pair still matches exactly. Claude itself allows the duplicate.
- Changed: with `sync_claude_session_name` on, the Claude session is the source of truth for a named agent — `hap agent rename` on one is reverted at the next capture. Rename such an agent through Claude's own `/rename`; hap will fold the name and write the folded form back.
- Note: with the setting on, a BRAND-NEW claude session is unnamed, so it is sent `/rename <hap name>` and your Claude conversation list fills with hap's generated animal names. It self-heals — `/rename` the session to something meaningful and hap adopts that instead.
- Fixed a composer detection bug that made `hap mode` refuse on every NAMED Claude session: the composer's top rule was required to end in two rule glyphs, and Claude renders exactly one after the session name.
- Changed the TUI to cap agent names at 15 characters in the Agents, Escalations and Audit tables. A synced name can run to 32 characters, which used to push every column after it out of alignment; the stored name is untouched, and the agent detail pane still prints it in full.

## 0.7.2

- Added `+` and `-` on the TUI *Rules* tab to nudge the selected rule's confirmation streak by one, and `hap signatures confirm <prefix> [--delta N]` as the same action from the CLI. Reaching `learning.graduation_n` graduates the rule only when its live confidence also clears the situation's threshold — the message says so when it does not, rather than looking like a key that did nothing.
- Changed how an autonomous rule can be demoted: `-` (or `--delta -1`) now walks the streak down and returns the rule to shadow once it falls below `learning.graduation_n`, keeping every decision row, the decision floor and the confidence. Previously `hap signatures reset` was the only way back and it cleared all three, so pulling a mostly-right rule back one notch meant making it re-earn trust from nothing. Nothing automatic demotes a rule; both paths are still explicit operator acts.

## 0.7.1

- Changed confirming a `[no_task_source]` escalation: it is a notice that an idle agent has no work queued, not an answerable prompt, so confirming it now shows how to enable LLM task generation (`hap config set llm.task_generate_command --preset claude|codex`) or add a task source, instead of failing with "no suggestion to confirm". The escalation stays pending; every other unconfirmable escalation still reports the old refusal.
- Fixed the TUI detail overlay losing rows off the bottom of a short pane when a message or status note was showing — its page size ignored both.

## 0.7.0

- Added `AUTO.md` at the repo root — the file hap's own consult and task-generation runs read, and `llm.learn_from_user_command` writes, kept out of the agents' own instruction files.
- **Breaking.** Removed the first-interaction command family: `llm.command_start`, `llm.task_generate_command_start` and their `_env`/`_env_file` companions. `llm.command` and `llm.task_generate_command` now serve every interaction. A config still carrying the old keys loads with one warning and is rewritten without them on the next save, so nothing breaks on upgrade.
- **Breaking.** Removed the fast-fail retry that re-ran a consult with the other command template when the first exited in under a second — it existed only to pair `command` with `command_start`, and there is no second template left to try.
- **Breaking.** An exhausted declared task source now always escalates `task_source_exhausted` with a confirmable `@noop` suggestion. Refilling it automatically used to be reachable by also setting `llm.task_generate_command_start`; rewriting a list you wrote is your call again.

## 0.6.21

- Changed the built-in `claude`/`codex` LLM presets to keep hap's learned lessons out of your agents' context. `llm.learn_from_user_command` now records a correction in `AUTO.md` in the agent's project — hap's own file, under one `## Lessons for hap's auto-answer assistant` heading it edits in place — instead of appending to that project's shared `CLAUDE.md`/`AGENTS.md`, where every rule was reloaded on every turn of the agent's real work. `llm.command` and `llm.task_generate_command` are told to read `AUTO.md` before deciding, so corrections still steer auto-answering — the codex task-generation recipe gains `--sandbox read-only` so it can actually open the file, and a run that cannot read it carries on without it. The rules in `AUTO.md` are framed as the operator's guidance rather than instructions that override the prompt, because the file is read from the agent-chosen directory and a cloned repo can ship its own. Existing configured commands are untouched; a preset only ever bootstraps an unset one.
- Changed the `llm.task_generate_command` preset to suggest only short-term work — the remaining steps of what is already underway on the agent's screen, or what closes it out — instead of open-ended "next tasks", which queued roadmap items onto a list the daemon then hands out unattended.
- Changed every preset prompt to open with "You are hap's auto-answer assistant."
- Rewrote `README.md` and the bundled `hap` agent skill (`hap skill`) to be roughly half as long, with each fact stated once. Corrected several stale claims: the default `llm.auto_act_confidence_threshold` is 85 (the skill's field table said 99); a pending escalation does not stop an idle agent's task hand-out (the README still said it did); full self-prompting is shipped, not on the roadmap; an exhausted task list escalates rather than sending a "none" prompt; a full-self-prompting acceptance is audited as `fsp-sent`, not `auto-sent`; and building from source needs the `vectors cpu` build tags.
- Added the previously undocumented surface to the agent skill: `hap gc`, `hap skill install`, `hap status --stderr`, addressing a task list by source index (`hap task 0 list`), `hap config set <llm command> --preset`, and the `escalations.auto_accept`, `logging`, `task_source_provider` and `llm.run_in_agent_cwd` config keys.

## 0.6.20

- Fixed a busy pane being reported as the runaway-limit verdict. Another interaction already driving an agent's pane now escalates as `pane_busy` instead of `rate_limited`, so it no longer pauses the agent until a human checks in, and no longer makes the escalation permanently operator-only.
- Changed `full_self_prompting.honour_limits = false` (the default) to switch the whole `[limits]` section off while the mode is active, rather than only skipping the mode's own pre-check. Neither runaway ceiling gates a send, a leftover runaway pause no longer benches an agent, and — since it is the third key in that section — `limits.max_error_retries` stops gating too, so an error signature retries without bound and `retry_exhausted` is never raised. Set `honour_limits = true` to keep the ceilings (and the stand-down they trigger) — note the counters keep advancing while inert and nothing resets the consecutive one, so turning the key on after a long unattended run trips that ceiling on the next sweep and switches the mode off. Nothing outside `[limits]` changes: the kill switch, per-agent disables, never-auto rules and the suspected-irreversible heuristic all still apply.
- Added a "not enforced while full self-prompting is active" note to the `limits:` line of `hap config show` when the mode is on with `honour_limits = false`. Conditional on purpose: `config show` reads config, and a mode enabled without its runtime preconditions has reverted to the ordinary flow, where the ceilings really are in force — `hap status` is what reports whether the mode is active.

## 0.6.19

- Added built-in `claude` and `codex` recipes for the three LLM commands that ship disabled (`llm.command`, `llm.task_generate_command`, `llm.learn_from_user_command`) — press `e` on a `(disabled)` row in the TUI's Config tab and pick a CLI, or run `hap config set <field> --preset claude|codex`. They install the same argv `sample/config.toml` documents, so turning the operator LLM on no longer means retyping a kilobyte-long prompt.
- A preset only bootstraps a command that is not configured: a field that already carries argv is refused and stays read-only in the TUI, so tuning one remains a `config.toml` edit and nothing can overwrite a template you wrote.
- Installing the `llm.task_generate_command` preset now says what it does not cover: refilling an exhausted declared task source additionally needs `llm.task_generate_command_start`, a separate opt-in that reads `(inherits …)` until you set it.
- `hap config set <field> --preset …` is dispatched on the FIELD, so `--preset` stays an ordinary literal value for every key that has no presets, exactly as before.

## 0.6.18

- Changed confirming an escalation with `--send`: the reply is now typed by the
  daemon instead of by whichever `hap` process you typed into. Two processes no
  longer drive the same agent pane, and an operator's answer finally goes
  through the never-auto screen and the per-agent lifecycle barrier that the
  daemon's own sends have always had. You still learn on the spot whether it
  landed.
- Changed `hap confirm --send` / `hap resolve --send` to require a running
  daemon, and to say so with the `hap daemon --ensure` remedy rather than
  recording a correction for a reply nothing will deliver. Confirming without
  `--send`, dismissing, and every config command still work with no daemon.
- Fixed a confirmed reply that a never-auto or suspected-irreversible rule
  refuses clearing its escalation anyway. Nothing was typed, the agent stayed
  blocked, and the row left the queue — so there was nothing left to look at.
  The refusal now leaves the escalation where a human will see it.
- Fixed confirming an escalation that was dismissed or resolved out from under
  you. The reply was delivered anyway and the row was flipped back to
  "resolved", overwriting whoever closed it. Both the queue and the daemon now
  refuse a closed escalation.
- Fixed a queued reply being typed at the wrong agent when herdr reused the
  pane id between the confirm and the send. The reply is bound to the terminal
  you answered against and refuses on a mismatch.
- Fixed a reply being sent twice when the daemon stopped between the keystrokes
  and recording the result. Such an action is now reported as "may or may not
  have reached the agent" instead of being replayed on the next start.
- Changed `--send` to refuse when the running daemon is OLDER than the `hap`
  you invoked: it may have no action drain at all, while its correction pass
  still resolves the escalation — losing the answer with nothing typed.
- Fixed hap opening the wrong SQLite database when its state directory path
  contained `?` or `#`. The path was pasted into a URI DSN unescaped, so it was
  truncated at that character — silently, with no error, and with every caller
  under such a path sharing one file somewhere else entirely.

## 0.6.17

- Changed every documented Claude Code recipe to pass `--no-session-persistence`, and every OpenAI Codex one to pass `--ephemeral`, so hap's background consults no longer land in your own session history or transcript store. The recorded `{session_id}` becomes a correlation key rather than a file you can open; drop the flag if you want the transcripts. Existing configs are unaffected — edit yours to pick this up.
- Fixed the claude prompt-adjacency repair bailing out on any template carrying `--no-session-persistence`, which would have left the prompt misplaced and failed the consult.

## 0.6.16

- Fixed a multi-tab question form whose captured question list was too long to store being escalated and then neither delivered nor dismissed, silently, forever — the capture was cut from the top, which is the half that says how many questions there are. Such a capture now gets a much larger storage budget, so it stays readable.
- Changed full self-prompting to answer a multi-tab form whose stored capture was already cut short, when the question on screen is one the surviving part of that capture still holds, the form is untouched, and the answer is one plain choice per tab. A form sitting on a question the cut removed still waits for you, as does anything else it cannot prove.
- Changed full self-prompting to answer an idle or generated-task escalation whose pane still shows the screen it was raised for, by comparing the two captures over the window they share instead of over their differing lengths. Below the tolerance the escalation still waits — this path never dismisses anything.
- Changed full self-prompting to retire a "do nothing" escalation instead of queueing it for an operator who, by definition, is not watching. Nothing is ever typed for one; it is recorded as dismissed, naming the reason.
- Added an info-level log line naming why the auto-accept pass left each escalation pending — once per escalation, and again only if the reason changes. Two of those refusals previously produced no output at any log level.
- Note for the first hours after upgrading: a multi-tab form escalation that was already pending may be raised a second time, because the stored capture was cut to the old, much smaller size and no longer looks like the same screen. It settles as soon as those older escalations are answered or dropped.
- Fixed an escalation being dropped from the queue when a safety rule refused the text a generated task would have sent: the refusal was counted as a failed delivery, so after three tries the escalation was discarded instead of waiting for you. It now waits, every time.
- Fixed pausing automation, or switching off either full self-prompting or its generated-task opt-in, not taking effect on an escalation already being checked — reading an agent's screen takes a moment, and an answer could still be sent inside that window.
- Fixed an escalation being able to disappear from the queue entirely if the database write that releases it failed: it was left in a half-answered state that neither the escalation list nor the automatic pass can see, and only a restart recovered it. Such a release is now retried until it succeeds.

## 0.6.15

- Fixed an idle agent's duplicate escalation costing a full task-generation LLM run before being discarded — the duplicate-ask check now runs before the generator is invoked, not after its result comes back. Measured on one agent in one morning, six of fourteen generation subprocesses were executed purely to be thrown away.
- Changed the escalation duplicate-ask window from 5 to 10 minutes, so a re-fire of a situation the operator just resolved no longer slips through as a second escalation when they come back to the pane a few minutes later.
- Fixed the startup warning for the retired `limits.escalation_dedup_window_seconds` key still telling operators the window is fixed at 5 minutes.

## 0.6.14

- Added `full_self_prompting.honour_limits` (default off): full self-prompting now checks the `[limits]` runaway ceilings BEFORE each delivery instead of noticing one decision later, and switches the whole mode off when one is reached — rewriting `enabled = false`, recording the change in `hap kill-history`, and notifying you which agent tripped it. The ceilings are per-agent while the mode is global, so one runaway agent stands the mode down for every agent.
- Added `full_self_prompting.accept_generated_task` (default off): full self-prompting may now act on an idle escalation whose suggestion is an LLM-generated task — writing the agent's task list, registering the task source and handing the first task over — instead of leaving it for you. It records no learning event, and the generated task text is screened against your never-auto patterns and the irreversible-command heuristic before anything is written; a match leaves the escalation for you. (Generated task text is authored after the decision that raised the escalation, so nothing had screened it before — your confirmation was the gate.)
- Fixed a task list being created under a remote provider even after the caller was cancelled — the create now aborts with its caller, and is retried on the next attempt.
- Added a `while_fsp_mode_on` flag to audit rows, so an automatic acceptance caused by full self-prompting is distinguishable from one caused by a timed auto-accept threshold expiring — they were previously identical in the log. Such rows now read `fsp-sent` instead of `auto-sent` in both `hap audit` and the TUI, render in amber on the TUI Audit tab, and name the cause in the detail view. Existing rows are not backfilled and keep reading `auto-sent`.

## 0.6.13

- Fixed automatic answering never firing on a Claude multi-tab question form. Because such a form is captured by sweeping every tab, while the staleness check re-read only the one tab on screen, every one of them was retired as "no longer on screen" — often within milliseconds of being raised, and while the form was still standing. The check now compares the visible tab against the tabs actually captured, and tolerates the form being left on a later tab or the highlighted option being moved while it waits.
- Fixed option labels absorbing the preview box that Claude draws beside them, which made an option's recorded text change whenever the highlighted preview did — so the same question kept being learned as a new one, and an option whose text wrapped was recorded as preview content instead. An option whose label wraps onto the row below its number is now still listed, rather than disappearing from the choices hap knows the agent is offering.
- Changed which multi-tab forms may be answered automatically: one that somebody has already started answering, one whose recorded reply is not a complete set of answers for it, and one whose capture was too large to store in full are now all left for the operator rather than answered.

## 0.6.12

- Moved the full self-prompting switch to its own top-level config group: `full_self_prompting.enabled` (was `escalations.full_self_prompting.enabled`). Nothing breaks — an existing config.toml still loads and the next save rewrites it under the new key, and `hap config set` still accepts the old spelling with a note naming the new one. It is now the first thing both `hap config fields` and the TUI Config tab show, in its own section.
- Added full self-prompting toggles to the automation history: turning the mode on or off now records an "FSP On"/"FSP Off" row alongside pause and resume, visible on the TUI Pause/Kill tab and in `hap kill-history`. Only toggles made through a hap surface are recorded; a hand-edited config.toml is not.
- Fixed the Pause/Kill tab running off the bottom of the pane: it now scrolls a fixed-height window like the other list tabs, shows how many rows are clipped, and supports `/` to filter by state, author or time. It also keeps 200 events in view instead of 50.

## 0.6.11

- Added full self-prompting mode: when enabled, every escalation that carries a proposed answer is accepted and delivered to the agent immediately — no waiting threshold, no operator action. The auto-accept safety exclusions are unchanged (never-auto matches, suspected-irreversible commands, retry-exhausted and rate-limited escalations still wait for you), it never acts while paused, it never learns from its own accepts, and each delivery counts against the `[limits]` runaway ceilings so an answer loop pauses the agent instead of running forever. Toggle with a double-press of `r` in the TUI or `hap config set escalations.full_self_prompting.enabled true`; enabling requires at least 10 graduated (autonomous) rules and a configured `llm.command`, and the refusal names whatever is missing. `hap status` and the TUI header show the mode, including an "ON but INACTIVE" state when a precondition later lapses.
- Added detection for Claude's per-model exhaustion banner ("You've reached your Fable 5 limit. Run /usage-credits to continue or switch models with /model."), which previously read as an ordinary idle screen and never raised an error. It now classifies as an `error` situation with its own `model-limit` summary — distinct from the account-wide `usage-limit` stop, because the remedy is switching model or buying credits rather than waiting for a reset — and it is recognised whatever status herdr reports, since a limit-stopped agent usually reports idle rather than blocked. Detection requires the banner to start its own line and to carry its slash-command remedy, so an agent merely quoting or narrating the message is not mistaken for a live stop.
- Fixed full self-prompting deliveries not being attributed to automation, which let an agent's own resume reset the consecutive-auto runaway counter moments after every accept — so the ceiling that pauses a looping agent for a human check-in could never trip.
- Added a moved-on check: an escalation is never auto-answered once the agent has gone back to work. The agent's status is re-read from herdr immediately before delivery rather than trusted from the moment the escalation was raised, so an agent that answered its own question, timed out its form, or was resumed by the operator is left alone instead of having text typed into whatever it is doing now. Such an escalation stays in your queue rather than being retired.
- Changed full self-prompting's immediate acceptance to run off the daemon's event loop, holding the same per-agent pane claim the form sweep and series delivery use — so one slow pane can no longer delay events for every other agent, and a full self-prompting answer can never interleave keystrokes with another delivery into the same pane.
- Fixed `hap status` reporting full self-prompting as active when the graduated-rule count could not be read: the daemon fails closed on that same query, so status now reports the mode inactive with the reason rather than claiming escalations are being answered when none are.
- Fixed two flaky daemon tests that could redden CI at random. `TestManualCaptureRecognizesIdleCodexPlanApproval`: the startup reconcile could escalate the pane before the test's manual capture ran, so the capture was recorded as a duplicate. `TestShutdownDrainsVerifyUnblockBeforeStoreClose`: it waited for a send count to equal exactly one while the same reconcile could produce a second send, so the count could skip the awaited value entirely.

## 0.6.10

- Added `hap --skill` (also `hap skill` / `hap skill show`): prints the bundled hap agent skill document — SKILL.md now ships inside the binary, so no repo checkout is needed to read it.
- Added skill installation for coding agents: `hap skill install <claude|codex|agents>...` and a Config-tab quick shortcut in the TUI (multi-select Claude / Codex / Others) write the bundled SKILL.md to `~/.claude/skills/hap/`, `~/.codex/skills/hap/`, or `~/.agents/skills/hap/`.
- Changed `hap pause` / `hap resume` (and the TUI `p`/`r` keys) to be idempotent: pausing while already paused or resuming while already running now reports "already paused/resumed" and records no kill-history event, so repeated presses no longer flood the history with no-op rows.

## 0.6.9

- Changed the default `learning.graduation_n` from 2 to 1 and `learning.confirmation_weight` from 3.0 to 2.0 — a rule now graduates to autonomous after a single consistent operator confirmation.
- Changed the default `escalations.auto_accept` thresholds for approval/choice/error from 15m to 5m (the feature itself still defaults to off).
- Changed the default `llm.auto_act_confidence_threshold` from 99 to 85, so a high-confidence LLM suggestion can auto-act out of the box instead of only a near-certain one.
- Changed the shipped never-auto seed list to cover only major-risk, hard-to-recover operations. Removed the strict patterns for locally recoverable or routine agent work — plain recursive `rm` (a narrow rule still escalates recursive deletion of `/` or the whole home directory), local git history operations (hard reset, clean, force branch delete, rebase), `chmod -R 777`, `terraform apply`/`pulumi up` (destroy still escalates), `docker … prune`, `gh release create` (delete still escalates), `gh auth logout`, `kill -9`, `systemctl stop`, shutdown/reboot, natural-language "delete all", "remove directory/folder" (volume/partition/bucket still escalate), `DROP INDEX` (TABLE/DATABASE/SCHEMA still escalate), and merging a pull request. Force-push, `sudo rm`, `dd`/`mkfs`/`shred`, database data loss, prod deploys, package publishes, cloud-resource deletion, credential rotation, and mass sends all remain.
- Removed two suspected-irreversible heuristics (a bare "are you absolutely sure" and overwrite/discard of changes/work) and dropped "remove" from the destructive-verb heuristic — routine agent prompts no longer trip false-alarm escalations; explicit no-undo language, "permanently delete", forced overwrites, credential invalidation, and prod/public publishing still do. Operators wanting any removed pattern back can re-add it via `safety.never_auto_patterns`.

## 0.6.8

- Fixed accepting an LLM-generated task escalation with a send under a `github_gist` task source, which failed with `stat gist://…: no such file or directory` after the list had already been created, the source registered and the escalation consumed — leaving the task in the list but never delivered. The send-time reservation was still reading the task list as a local file.
- Fixed the choice of which of an agent's task sources receives generated tasks when several match: gist-backed candidates were unreadable to that check, so it silently fell back to the first source instead of the one with pending work. A candidate that cannot be resolved is now reported instead of being skipped in silence.
- Fixed a second confirm for the same agent duplicating its own tasks under a gist source — the agent's own list was mistaken for another source's, and the tasks were appended again without their numbering.
- Fixed a task left stuck in progress when delivery failed because the operator quit: returning it to pending no longer depends on the cancelled request that killed the delivery.

## 0.6.7

- Fixed `hap update` naming a stale version: the "installing …" line now does a live release check first and uses the cached record only when the fetch fails, so a cache written before a release published can no longer misname the target.
- Changed `hap update`'s closing line to report the version read back from the installed binary itself — including the case where install.sh fell back to an earlier release's assets — with a note to retry when the newest release's assets were not published yet; when nothing can be read back it says "install finished" instead of guessing.

## 0.6.6

- Fixed accepting an LLM-generated task escalation against a `github_gist` task source, which always failed with `422 Validation Failed` and created no task list. hap created the agent's list blank and filled it a moment later; GitHub cannot store a blank gist file at all, and its refusal named neither the list nor the cause. The list is now created with its header, the way every other create-on-demand path already did.
- Added a clear refusal when a task list would be written blank to a gist, in place of GitHub's `422 ... Field:files`, so emptying a list reports what is wrong instead of a malformed-request error.

## 0.6.5

- Fixed `hap task <agent> add` refusing to create a task list that does not exist yet: a configured source's list is now created on demand, so a gist-backed source can be seeded from the CLI or the TUI. Before this a fresh remote source was a dead end — there is no file to create by hand, and the list was only ever created by the daemon's first hand-out, which cannot happen until the list holds a task. `--path` still refuses a missing file, so a typo fails loudly.
- Fixed every `hap config …` write reporting failure (exit 1, success output suppressed) when the daemon is not running, even though the change had been saved — the ordinary first-run order, configure then start, made every command look broken and invited re-running it.
- Fixed the TUI Tasks tab hiding a task list whose agent is not currently running: a source scoped to the name of an agent hap has seen before now shows its list whether or not that agent is live, matching what `hap task <name> list` has always printed. A selector hap has never seen as an agent name still shows the per-agent note, because a task-source selector can equally name an agent TYPE, whose lists are genuinely one per agent.
- Fixed the TUI Config tab showing no storage location for gist-backed task sources — a derived source rendered a blank column that read as unconfigured. Each source now names where its list lives, as `hap config task-source list` does.
- Changed `hap config set` and `config set-threshold` to say whether the change reached a running daemon: they claimed "(daemon reloaded)" unconditionally, including when no daemon was running. The TUI's field editor says the same.
- Fixed a gist-backed list failing to be used right after it was created: GitHub's gist reads are not read-after-write consistent, so the read that follows a create could still report the list missing (measured live: one miss in three rounds). The store now re-reads briefly, and only for a file it wrote itself, so a typo'd name still fails immediately. This affected both the CLI's create-on-demand add and the daemon's first hand-out.
- Added task creation from the TUI for a list that does not exist yet — previously only the CLI could create one, though the TUI's own refusal message pointed at the feature.
- Fixed `max_tasks` being ignored when a task was added from the TUI: the cap lookup and the create path now resolve the same source, so a limit applies however the list is addressed.

## 0.6.4

- Fixed the TUI Tasks tab for gist-backed task sources: a derived (one-list-per-matched-agent) source now shows its actual checklist when exactly one live agent matches it, instead of the "one list per matched agent" note; and task actions (done, edit, delete, move, send, add) now address remote lists by their resolved locator, so they no longer no-op or resolve a gist file name against the local filesystem.
- Added a provider-independent `hap task` selector: a task source is now addressable by its config index (`hap task 0 list`, also `'#0'`) — the way to reach a source an agent name cannot (workspace- or type-scoped, an agent matching several, or any list under a remote provider, where `--path` reads a local file that does not exist). `--path` still works for ad-hoc local files.
- Changed the default next-task prompts and the `hap task … list` hints to offer the task-source index as the fallback selector instead of `--path`; a new `{task_source_index}` placeholder is available to custom templates (it falls back to the agent name when the sender cannot know the position).

## 0.6.3

- Added `path`, `agent`, `workspace` and `template` to `hap config task-source set`, so every field of a task source is now editable in place. Changing one of them used to mean removing the source and re-adding it — retyping every other field, and renumbering every later source, to change one. The three that re-point a source report what they changed from and say so plainly: the next hand-out comes from a different list, or goes to a different agent. Nothing is copied or removed either way; an empty agent or workspace matches any of them and says so; a relative path is resolved against your shell's working directory (the daemon runs from the state dir); and a whitespace-only template clears back to the built-in default rather than being delivered as the prompt.
- Changed `hap config task-source set` and `remove` to accept the AGENT NAME a source feeds, not only its positional index — the index is what `list` prints, but it moves whenever an earlier source is removed, so a number you remembered can silently mean a different entry. `#0` is still accepted verbatim so a listing row can be copied without editing it; a name matching no source, or more than one, is refused naming the indexes that disambiguate it.
- Changed `hap config task-source list` to end with follow-ups that name a REAL source rather than telling you to run the listing you just ran, and the empty listing now points at `add` instead.
- Fixed `hap config task-source add --agent 3` and `set <ref> agent 3` being accepted: a bare number is how the CLI addresses a source by INDEX, so a numerically-named source was permanently unaddressable by name while the same token silently meant a position. Refused where it would be written; a real pane or agent id (`1-1`) is unaffected.

## 0.6.2

- Changed the installer to fall back to the newest earlier release when the version the plugin declares has no downloadable assets yet, so `herdr plugin install` and `hap update` keep working during the ~15 minutes a release spends building (and indefinitely if that build fails) instead of failing with a 404. The install says loudly which version it actually installed; run `hap update` once the intended one publishes.
- Added `HAP_NO_FALLBACK` (any non-empty value) to refuse the substitution and fail instead, for reproducible installs. Pinning an exact version with `HAP_VERSION` never falls back either, and a checksum mismatch still fails hard rather than quietly installing something older. Note a `--ref` pin is not one of these: it pins the git clone, which the install step cannot see. The substitution is only ever for a release whose assets are not published — once they download and verify, a failure to unpack or install them reports that problem instead of quietly fetching an older version.
- Fixed a failed install leaving a half-swapped plugin directory — the previous binary and `lib/` now survive intact, because nothing is replaced until every required asset has been downloaded, verified, and unpacked.

## 0.6.1

- Added `hap config classifier` — list, add and remove the operator rules that decide which situation a pane is showing. These were settable in config.toml only, so a screen hap read as the wrong situation could not be corrected from a shell.
- Added `hap config capture-delay` — read and set how long the daemon waits after a herdr event before reading a pane, per agent type. The listing resolves the built-in defaults, so it shows the delays actually in force rather than only the overrides; setting a type that already has a rule overwrites it, since the daemon reads the first matching rule and a second would never be reached.
- Added `hap config rules add --agent-type` and `hap config rules remove-scoped` — agent-scoped never-auto rules could be listed but not created or deleted from the CLI. A wildcard scope is refused: that is what the unscoped list already means.
- Added `hap config env` — set, unset and list the environment handed to the LLM CLI, per command scope. Values are never printed by any listing and `set` reads the value from stdin unless `--value` is passed, so an API key never lands in shell history or another user's `ps` output.
- Added `hap escalations retry <id>` — re-invoke the LLM on a consult that failed or timed out (and re-run a failed learn-from-correction). This was previously possible only from the TUI.
- Added `hap status --stderr` — print the captured daemon stderr, not just the one-line summary the health line quotes.
- Fixed `[[capture_delay]]` matching an agent type case-sensitively — the one place in hap where an operator's capitalization silently mattered. `agent_type = "Claude"` wrote a rule the daemon never read while every listing showed it in force; the match now folds case like every other agent-type comparison.
- Changed: everything that writes config.toml is now a `hap config` subcommand — `hap rules`, `hap task-source`, `hap classifier` and `hap capture-delay` became `hap config rules`, `hap config task-source`, `hap config classifier` and `hap config capture-delay`. The old spellings still work and print a note naming the new one on stderr, so scripts parsing these listings keep working unchanged. `hap task` deliberately stays top-level: it edits checklist items in an agent's markdown file, not configuration.
- Changed: every configuration key config.toml accepts now has a CLI command, and three tests fail the build if a new one ever ships without one, or outside `hap config`.

## 0.6.0

- Added a provider column to `hap task-source list` and a task-store line to `hap status`, both shown only once something selects a non-default storage backend — an install that never touched the setting sees exactly the output it always did. Each row says whether its provider is inherited from the default or overridden on the source, because an inherited value and an identical override behave differently the next time you change the default.
- Added `provider` and `gist_id` to the TUI Config tab's task-source editor (`enter` on a source row), including an `inherit` choice that puts a source back to following the default.
- `hap status` now names a misconfigured task store and why it cannot be reached, in the same words every other surface uses.
- Added a task-list storage provider: set `[task_source_provider] provider = "github_gist"` and a task source's checklist lives as a file inside a GitHub gist you own instead of on this machine, so a herd spread across hosts shares one list. Off by default, and the setting is a DEFAULT — each `[[task_sources]]` entry can override it, so some agents can keep their lists local while others are in a gist at the same time.
- Changed what a task source's `path` means under a remote provider: name a file to share one list across every agent the source matches, or leave it out and each matched agent gets its own list named after it, created the first time it is handed a task.
- Added `hap task-source provider`, which shows where lists are stored, which gist, and whether the credential file resolved — never the token itself.
- Changed the next-task prompt under a remote provider: it no longer offers `--path`, which always reads a local file and would have pointed the agent at something that does not exist.
- Fixed a generated task list being overwritten when hap could not read it. An unreadable-but-present list was treated as empty and then replaced, discarding every task in it; the read now happens inside the locked update and a failure leaves the list untouched.
- Fixed two simultaneous generated-task confirmations racing to create the same list, where the second one's write discarded the first one's tasks.
- Changed the privacy promise: hap still makes no outbound call in its default configuration. Enabling the gist provider sends the task lists of the sources using it — and only those task lists, never pane content, learned rules or the audit log — to a gist you own, using a token you supply.
- Changed how a stranded task hand-out is returned to the pending list when the task source is stored remotely: the release now happens in the background and its result is settled on the daemon's next turn, instead of the daemon waiting on the network once per stranded row. A backlog of stranded hand-outs no longer delays every other agent's classification and delivery. Local task lists are unaffected — they are released exactly as before.

## 0.5.34

- Added `hap mode <agent>` to print an agent's permission mode, and `hap mode <agent> <mode> [--yes]` to set it — claude offers `manual`/`acceptEdits`/`plan`/`auto`, codex `default`/`plan`. Setting is idempotent: an agent already in the target mode receives no keystroke and is not even prompted.
- Added a `mode` column to `hap agents` and a `Mode` row to the TUI agent detail view. The column is appended after the working directory, so existing field positions are unchanged. Both show `-` when the mode could not be read, never a guessed default.
- `hap mode` refuses instead of guessing when an approval or form is covering the agent's composer footer: inside those modals shift+tab means "approve with this feedback", so pressing it there would answer the prompt rather than change the mode.
- A mode a session does not actually offer (a `--model haiku` claude has no `auto` mode) now fails as soon as the cycle closes and rotates the agent back to where it started, instead of pressing to the ceiling and leaving it in an arbitrary permission mode.

## 0.5.33

- Changed the LLM CLI to run in the monitored agent's own working directory instead of hap's, so consults and task generation read that project's `CLAUDE.md` / `AGENTS.md`, see its local tool config, and can resolve repo-relative paths. An unknown or deleted directory falls back to the previous behavior rather than failing the run.
- Added `llm.run_in_agent_cwd` (default `true`) to turn that off and keep running the CLI where hap runs. It does not affect `llm.learn_from_user_command`, which already required the agent's directory.
- **Breaking.** Added `--strict-mcp-config` to any `claude` command that passes `--mcp-config`, so the MCP servers hap names are the complete set for that run. The agent project's own `.mcp.json` can no longer add servers to a decision — and neither can your user-level `~/.claude.json`, `--settings`, or enabled plugins, which reached the consult before and now do not. Move any server you want to keep into the `--mcp-config` JSON. A command that passes no `--mcp-config` is left alone. `codex` is unaffected: it has no such flag and reads MCP servers only from `$CODEX_HOME`, so a project directory cannot add any.

## 0.5.32

- Added `llm.learn_from_user_command`: when you correct an escalation, hap runs a one-shot CLI in the agent's own working directory and asks it to record the lesson in that project's memory file (`CLAUDE.md`, or `AGENTS.md` for codex), so a correction outlives the one screen it was learned on. Off unless configured. Confirming hap's suggestion never triggers it, the run never touches the pane and never escalates, `hap pause` suppresses it, and every run leaves one `hap audit` row (`llm-learn-from-user`) carrying the CLI's stdout and stderr verbatim, so you can read what it did — press `v` on the row in the TUI's Audit tab. Nothing is parsed out of the reply, so the prompt needs no sentinel. A failed run is retryable with `l` on that same detail view; the retry is refused when the agent's pane is gone or now runs a different agent, and while automation is paused. The run is refused (and audited as `learn:failed`) when the agent's working directory cannot be resolved, so a file-editing CLI is never pointed at an unrelated project. `llm.learn_from_user_timeout_seconds` bounds a run and inherits `timeout_seconds` when omitted.

## 0.5.31

- Added tests pinning `codex` session-id extraction against a verbatim capture from a real codex-cli 0.146.0 run, including the case where codex prints an unrelated error carrying the same UUID in a file path *before* its banner — where a looser pattern would read the wrong id on a completely ordinary run.
- Documented where each LLM CLI writes its session transcript, since `claude` and `codex` do not agree and a lookup cannot assume one layout.

## 0.5.30

- Fixed the session id being read from the truncated copy of an LLM CLI's output rather than the whole of it. For a CLI that reports its own id (`codex`), enough output before the announcement would push it past the 16 KiB the audit row keeps, and hap would silently record the id it had minted instead — naming a conversation that never existed, with no error to explain it.

## 0.5.29

- Fixed the LLM session id being recorded only on escalations. A decision the LLM answered and hap delivered — the most common outcome — left its audit row blank, so exactly the rows most worth tracing could not be tied back to the transcript behind them. Every audit row carrying LLM detail now carries the id: delivered actions and no-ops, multi-tab and remote-environment answers, task-list reviews, and the row written when an agent was disabled mid-flight.

## 0.5.28

- Added a session id to every LLM invocation, recorded on the audit row it produced. A decision can now be traced to the transcript the CLI wrote for it — previously nothing linked the two, so there was no way to tell which consult produced a given escalation.
- Added the `{session_id}` command placeholder. For `claude`, hap appends `--session-id {session_id}` on its own; write the placeholder yourself only to place it differently, which turns the automatic injection off. `codex` has no such flag and is never passed one — hap reads the id back from its startup banner instead. Any other CLI gets nothing added, since a guessed flag name would fail every consult.
- The id is recorded for failed consults too: a timeout or a no-submit still wrote a transcript, and still raises an escalation.

## 0.5.27

- Added `hap gc` — reclaims disk from hap's own records on demand, with `--dry-run` to see the window first and `--days N` to override it. It blanks the captured pane excerpt on aged audit rows (the bulk of the database: ~3.8 KiB of a 5.0 KiB row) while keeping the rows themselves, so `hap audit` history stays complete. Rows the daemon may still read are never touched — pending escalations at any age, rows with an unprocessed LLM retry, and recently answered asks.
- Added a daily retention sweep so the audit history stops growing forever. Nothing pruned it before, and a lightly used state directory grew about 0.6 MB a day. Tune it with `[logging] audit_excerpt_retention_days`: omitted keeps 14 days, `0` keeps no excerpts at all, and a negative value never prunes (the old behaviour).
- Added `[logging] level` and `[logging] max_size_mb`. There was previously no way to turn the plugin log down — only `HAP_DEBUG=1` to turn it up, and that never applied to `hap tui`, which logged into the same file. The default log cap drops from 64 MiB to 16 MiB, so an untouched install reserves 32 MiB instead of 128 MiB counting the `.old` sibling.
- Fixed hap going blind to agent status for up to 30 seconds after an ordinary pane split. Every pane open, close, split and agent-detection unwinds the status subscriber on purpose, but that was treated as a dropped connection: it logged a warning each time and advanced the reconnect backoff to its 30 second ceiling. Expected resubscribes now reconnect immediately and log at debug.
- Fixed `daemon.stderr.log` growing without bound for a daemon that never restarts — its 256 KiB cap was only checked when a new daemon was spawned.
- Changed the embedding worker to stop writing llama.cpp's model-load banner to that log: roughly 250 lines every time the worker started, which is what the file was almost entirely made of. It now keeps the native crash trail it exists for. Set `LLAMA_LOG` yourself to override.
- Changed several routine daemon lines to debug — successful signature matches, and the once-a-minute note that an idle agent has no task waiting. Both described a steady state and together dominated the log.
- Fixed the write-ahead log staying at its high-water mark forever; it is now truncated back after each checkpoint.

## 0.5.26

- Added an idle back-off to `hap tui`: after 10 minutes with no keypress and nothing changing, it polls every 30 seconds instead of every 2, and repaints the Age column every 10 seconds instead of every 1. A pane left open overnight stops costing a full store read and two herdr round trips every 2 seconds — measured at about a third of the idle CPU it used before.
- Any keypress or pane resize returns it to the live cadence at once and refreshes immediately. Agent status changes, new escalations, task-list progress, and daemon health changes also restore it, but are only noticed by the poll that finds them, so while backed off they can be up to 30 seconds late — including the TUI's own escalation bell. The daemon raises its own herdr notification independently, so no alert is lost.

## 0.5.25

- Fixed `hap tui` burning a quarter of a CPU core the whole time it was open, doing nothing. Listing learned rules re-read and re-parsed `config.toml` once per rule, so each 2-second refresh spent most of its time decoding the same file over and over. Measured on a 196-rule state directory, an idle TUI now costs about a sixth of what it did.
- Changed the Rules listing to read every rule's decision history and totals in two queries instead of two per rule. `hap signatures list` and `hap audit` are faster by the same amount.
- Changed `config.toml` loading to parse the file twice instead of eight times — it used to re-parse the whole file for each deprecated-key check. This applies to every `hap` command and to the daemon, not just the TUI.
- Added a warning when a deprecated `[thresholds]` table is present but cannot be read. Its values were previously dropped in silence on the next save.

## 0.5.24

- Fixed the TUI Tasks tab lagging behind its own actions: toggling done (`d`), deleting (`x`), adding (`a`), and editing (`e`) a task now update the list immediately — the checkbox flips at the keypress and the write's own result refreshes the rows — instead of showing the old state for seconds until the next full refresh, which used to invite a second `d` that flipped the task straight back.

## 0.5.23

- Fixed an approval answer selecting the wrong option: Claude Code renders "Yes, and don’t ask again" with a typographic apostrophe (U+2019) while every learned rule and LLM answer writes the ASCII "don't", so the label matched nothing and the reply was typed as literal text — which a standing menu ignores, leaving its Enter to commit the first option ("Yes"). Option labels now compare with typographic punctuation, case and whitespace folded away.
- Changed a reply that matches none of the options a menu is offering to escalate instead of being delivered. It used to fall through to a literal send whose Enter committed whichever option the caret rested on — always the first — silently and with a success exit code. This covers the autonomous, LLM, LLM-rewrite and operator-confirm paths alike.
- Changed an unreadable pane to refuse a menu answer when the decision's own capture shows a menu was on screen, rather than sending the reply blind.
- Fixed a label that appears at two different numbers in one capture (two renders of a menu in the same screen) being answered with a digit from the stale render; it is now refused as ambiguous.

## 0.5.22

- Fixed every reply to an agent failing with `herdr agent send: exit status 2` on herdr 0.7.5, which removed that command. A single-line reply is now typed with `pane send-text` and submitted with Enter, and a multi-line one is delivered with `agent prompt`; older herdr (0.7.0–0.7.4) still gets the previous `agent send` + Enter pair.
- Fixed the routing so an approval answer keeps working: a menu digit has to reach the agent as a keystroke, and delivering it as pasted text answers whichever option the cursor happened to be on instead of the one that was chosen.
- Fixed a stray Enter reaching Codex after a multi-line message: the delayed second Enter that works around Codex swallowing the first one now only fires when hap actually pressed Enter itself, so it can no longer submit a blank turn or accept whatever control Codex has on screen.
- Changed the refusal you get when confirming an escalation that carries no suggestion: it now names the safety control that withheld it and the exact `hap resolve` / `hap dismiss` command to use, instead of only saying there was nothing to confirm.
- Fixed the plugin log growing without bound (a live state directory reached 1.9 GB). It now rotates to a single `.old` sibling at 64 MiB, so at most 128 MiB is kept.
- Changed config deprecation warnings to be logged once per process instead of on every config reload, which is what filled that log.
- Fixed the local integration suite failing to start agents on herdr 0.7.5, which reshaped `agent start` and caps agent names at 32 characters; failures now report herdr's own stderr instead of a bare exit status.

## 0.5.21

- Fixed a test that could fail spuriously on a busy machine, making CI runs fail for reasons unrelated to the change under review.

## 0.5.20

- Fixed `embedding.bm25_highbar_score` being invisible to `hap config` and the TUI config screen. It shipped settable in `config.toml` but `hap config set` rejected it as an unknown field, so the only way to change it was editing the file by hand. It is now listed and editable in both.
- Added the eight `tui.palette.*` color roles to `hap config fields` and `hap config set`, which previously reached every other config key but not these. Values are validated — an unrecognized color renders as no color at all, and these roles are hidden from the TUI config screen, so a rejected value is the only feedback available. Setting a role to `""` clears it back to the selected theme.
- Added a check that every key `config.toml` accepts is reachable from `hap config set`, so a new setting can no longer ship configurable in the file but unknown to the CLI.

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
