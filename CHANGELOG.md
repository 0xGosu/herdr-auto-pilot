# Changelog

Every change gets a line here, including patch releases — see the Changelog
section in `CLAUDE.md`.

**This file is assembled, not edited.** Contributors add a fragment in
`changelog.d/` (one file per PR, so two PRs can never conflict); the release
automation folds those into a new section here under the version it actually
assigns. Do not add a heading or an entry by hand.

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
