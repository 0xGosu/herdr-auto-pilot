---
name: hap
description: "Control the Herd Auto Prompter (hap) plugin from Herdr. Check automation status, manage escalations, configure thresholds, manage agents, task sources, and safety rules — all via the hap CLI. Use when the user asks about auto-prompting, escalations, agent monitoring, or the hap plugin."
---

# hap — Herd Auto Prompter agent skill

the `hap` binary is the CLI for the **Herd Auto Prompter** herdr plugin. it watches every agent session in the herd, detects when an agent needs input (idle, waiting on approval, stuck on a choice, or stalled on an error), and automatically supplies the next prompt — learned from the operator's own past decisions.

before using this skill, confirm the plugin is installed:

```bash
herdr plugin list --json | grep herd-auto-prompter
```

the `hap` binary lives inside the plugin directory. find it with:

```bash
PLUGIN_ROOT=$(herdr plugin list --json | python3 -c 'import sys,json; plugins=json.load(sys.stdin)["result"]["plugins"]; [print(p["plugin_root"]) for p in plugins if p["plugin_id"]=="herd-auto-prompter"]')
HAP="$PLUGIN_ROOT/bin/hap"
```

if `hap` is already on `PATH`, use it directly.

## concepts

**situation types** — the plugin classifies agent states into four types:
- `idle` — agent finished and is waiting for the next prompt
- `approval` — agent is asking for permission (e.g. tool approval)
- `choice` — agent presents a multiple-choice question
- `error` — agent hit an error and is waiting for guidance

**shadow mode** — the plugin starts by observing and escalating with suggestions. the operator confirms or corrects. after enough consistent confirmations, a situation signature "graduates" to autonomous.

**escalation** — when confidence is below the threshold, the plugin surfaces the situation to the operator instead of acting automatically.

**never-auto patterns** — destructive operations (force-push, `rm -rf`, deploys, credential changes, etc.) are never automated regardless of confidence. the plugin ships with 38 strict seed patterns plus broader heuristic seed rules.

**signatures** — a situation signature is a fingerprint of a classified agent state (volatile data like paths, hashes, timestamps is masked). signatures start in shadow mode and graduate to autonomous after enough consistent confirmations. you can inspect, filter, and delete them via the `signatures` command (alias: `sigs`).

**semantic matching** — situations are matched to learned signatures using embedding-based vector search (bundled MiniLM model via llama.cpp). paraphrased prompts reuse the learned rule instead of re-learning from zero. when the model is unavailable the daemon falls back to normalized BM25 text matching, then exact hash matching — it never blocks or crashes on missing assets.

**@noop** — a special action meaning "no reply is needed." the agent finished or is only reporting status. a noop is recorded in the audit trail and learned like any other decision, but nothing is sent to the pane. use `resolve <id> --action @noop` to teach the plugin that a situation needs no response.

## architecture — how the daemon talks to agents

hap is not a wrapper around the coding agents; it observes and drives them from the outside, entirely through herdr. herdr is the terminal multiplexer that runs each agent (claude, codex, agy, …) in its own pane and exposes a local unix-socket API. hap plugs into that API.

**one long-lived daemon.** a single `hap daemon` process monitors every agent pane in the herd. it is a singleton — auto-started (and self-replaced on upgrade) by herdr firing the plugin events `pane.agent_detected` and `workspace.created`, both wired to `hap daemon --ensure`. the CLI commands in this skill (`hap status`, `hap confirm`, …) are thin front-ends that talk to the daemon over its control socket; they never drive agents directly.

**the monitor loop** — for every agent, the daemon runs this cycle:

1. **subscribe** — one `events.subscribe` per socket connection to herdr's event stream; the daemon receives a status/attention event whenever an agent's pane changes (idle, waiting on a prompt, etc.). existing panes replay as `pane_created`.
2. **capture** — on an attention event the daemon waits a short `capture_delay` (10s on an agent's first event, 500ms after) so the agent's TUI has finished painting and event bursts coalesce, then reads the pane content via the herdr CLI (`pane read --source recent` for classification, `--source visible` to recover a standing menu at confirm time).
3. **classify** — the pane text is classified into a situation type (`idle` / `approval` / `choice` / `error`), extracting options, permission verb, or error summary.
4. **match** — volatile data (paths, hashes, timestamps) is masked and the situation is resolved to a learned signature via embedding vector search → BM25 text fallback → exact hash. a matched signature carries the operator's past decision and a confidence.
5. **decide** — if a confident, graduated (autonomous) rule applies and passes every safety gate (kill switch, never-auto patterns, irreversible heuristic, rate/retry guards), the daemon **acts**. otherwise it **escalates** to the operator — or first **consults the local LLM** (if configured), whose suggestion is re-gated through the same safety controls.
6. **act** — the chosen reply is delivered back into the pane through herdr: `agent send <pane> <text>` writes the text, then `pane send-keys <pane> enter` submits it (herdr's `agent send` does not press Enter on its own). a numbered menu (`1. Yes / 2. No`) is mapped to its **digit** via `MenuKeystroke` before sending — herdr menus accept the number, not the label. a `@noop` decision records/learns but sends nothing.

**shadow → autonomous.** a new signature starts in shadow mode: the daemon escalates with a suggestion, the operator confirms or corrects, and after enough consistent confirmations (`learning.graduation_n`) the signature graduates and the daemon acts on it unattended. an operator confirm/send weighs extra toward confidence (`learning.confirmation_weight`, default 3×), so confirmed rules build confidence faster. **graduation is permanent**: once autonomous the confirmation count is frozen and a later correction no longer demotes the rule (the correction is still recorded, so confidence — and the confidence gate — reflect it). the only way back to shadow is an explicit `hap signatures reset` (or the Rules-tab reset key).

**everything is out-of-process and fail-safe.** the daemon never blocks the agent and never modifies the agent's process — it only reads panes and sends keystrokes, exactly as a human operator would. shelling out to the LLM CLI or doing deep pane reads happens in goroutines so the single select loop keeps serving all agents; any error resolves to escalate + audit, never a crash.

## check status

see overall automation state, pending escalations, and monitored agents:

```bash
hap status
```

output:
```
automation:          running
daemon:              running v0.3.13 (pid 50998)
pending escalations: 0
monitored agents:    6
semantic matching:   ready (1 signatures, all-MiniLM-L6-v2-f32.gguf)
last kill event:     resumed by operator at 2026-07-11T21:49:30+08:00
```

`hap status` shows the running daemon's version and flags a stale one after an upgrade. it can also print an embedding-drift line (`run: hap signatures reembed`) when the stored embeddings no longer match the active model.

## list agents

see all monitored agents with their short names, pane ids, types, and statuses:

```bash
hap agents
```

output:
```
brave-otter  w6:p1   claude   idle
cool-fox     w6:p3   claude   working
swift-hawk   w8:p1   codex    done
```

## rename an agent

give an agent a friendly name (used by task sources and for readability):

```bash
hap rename brave-otter backend-dev
```

## manage escalations

list pending escalations (situations the plugin is not confident enough to handle automatically):

```bash
hap escalations
```

confirm an escalation's suggested action:

```bash
hap confirm <id> --send
```

- `--send` also delivers the confirmed action to the agent pane immediately

provide a different (correct) action instead:

```bash
hap resolve <id> --action "yes, proceed" --send
```

- without `--send`, the action is recorded for learning but not sent to the agent
- use `--action @noop` to record that no reply was needed (nothing is ever sent)
- `hap correct` is an alias for `hap resolve`

**once a situation graduates to autonomous, let the daemon answer it — don't
also drive the pane by hand.** confirming (`--send`) an approval a few times is
how a shadow signature earns its way to autonomous; after it graduates the
daemon auto-answers matching prompts itself, delivering the numbered-menu digit.
if you *also* type the menu digit into the pane you race the daemon: both
keystrokes land, one picks the menu and the extra digit lands in the input box
as stray text. respond through `hap` (`confirm` / `resolve` / `dismiss`), not by
typing into the agent pane, and once a rule is autonomous leave its approvals to
the daemon.

dismiss pending escalations without responding (audit rows kept, nothing sent or learned):

```bash
hap dismiss <id>
hap dismiss <id1> <id2> <id3>
```

prune old escalations — dismisses all pending escalations older than N minutes (default 360):

```bash
hap escalations prune
hap escalations prune 120
```

## pause and resume (kill switch)

pause all automation globally:

```bash
hap pause
```

resume automation:

```bash
hap resume
```

while paused, situations still classify and escalate — nothing is auto-answered — and those
escalations carry the rationale `[daemon_paused]`, meaning "the operator paused automation",
not that anything crashed.

view pause/resume history:

```bash
hap kill-history
```

## view audit log

see the history of automated actions and escalations:

```bash
hap audit
hap audit --limit 50
```

every automated action and every escalation writes an audit record with: trigger, situation, action, confidence, and rationale.

correct a past automated decision (demotes the signature back to shadow mode):

```bash
hap resolve <audit-id> --action "the correct response"
```

## learned signatures

inspect what the plugin has learned. signatures are addressed by unique prefix, git-style (e.g. `approval:9f2c`).

list all learned signatures:

```bash
hap signatures list
hap sigs list              # alias
```

filter signatures:

```bash
hap signatures list --type approval          # by situation type (idle|approval|choice|error)
hap signatures list --mode autonomous        # by mode (shadow|autonomous)
hap signatures list --agent-type claude       # by agent type
hap signatures list --min-conf 0.85          # by minimum cached confidence
```

show full detail for a signature (recent decisions, last audit context):

```bash
hap signatures show approval:9f2c
```

delete a signature you no longer trust (erases its decision history; audit rows are kept):

```bash
hap signatures delete approval:9f2c --yes
```

reset a signature back to a fresh rule: shadow mode, streak → 0, and confidence back to
unscored — a reset rule reads `conf=-`, not a number, because confidence counts only
post-reset decisions and there are none yet. all decision rows are **kept** (and the rule
still suggests its learned answer),
but decisions recorded before the reset no longer count toward confidence or graduation — the
rule behaves confidence-new and must re-earn `learning.graduation_n` confirmations to
re-graduate. this is the only way to demote an autonomous rule now that graduation is
permanent:

```bash
hap signatures reset approval:9f2c --yes
```

re-compute stored embeddings after changing the embedding model:

```bash
hap signatures reembed
hap signatures reembed --force    # retry even when no drift is detected
```

## configuration

show current config:

```bash
hap config show
```

list all configurable fields:

```bash
hap config fields
```

print the config file path (even before the file exists):

```bash
hap config path
```

set a specific config field:

```bash
hap config set <field> <value>
```

examples:

```bash
hap config set learning.graduation_n 3
hap config set limits.max_consecutive_auto_prompts 10
hap config set limits.max_auto_prompts_per_minute 20
hap config set limits.max_error_retries 3
hap config set llm.timeout_seconds 120
hap config set llm.auto_act_confidence_threshold 80
hap config set llm.pane_excerpt_chars 8000
hap config set safety.disable_never_auto_seed_patterns true
hap config set embedding.disabled true
hap config set embedding.similarity_threshold 0.85
hap config set embedding.gpu_layers 4
hap config set embedding.model_context_window 512
hap config set embedding.pane_salient_chars 1000
hap config set tui.max_content_width 120
hap config set tui.theme high-contrast
```

set a confidence threshold for a situation type:

```bash
hap config set-threshold <situation> <value>
```

examples:

```bash
hap config set-threshold idle 0.70
hap config set-threshold approval 0.85
hap config set-threshold choice 0.90
hap config set-threshold error 0.80
hap config set-threshold inferred_task_bar 0.95
```

edits made through `hap config set` / `set-threshold` apply live — the command saves and nudges the running daemon to reload. a hand-edited `config.toml` is NOT auto-detected (there is no file watcher); it takes effect only when another CLI/TUI command triggers a reload or the daemon restarts.

### configurable fields reference

| field | default | description |
|---|---|---|
| `confidence_thresholds.minimum` | 0.50 | variance guard minimum learned-action agreement |
| `confidence_thresholds.idle` | 0.65 | confidence threshold for idle agents |
| `confidence_thresholds.approval` | 0.70 | confidence threshold for approval requests |
| `confidence_thresholds.choice` | 0.70 | confidence threshold for choices |
| `confidence_thresholds.error` | 0.75 | confidence threshold for error situations |
| `confidence_thresholds.inferred_task_bar` | 0.60 | higher bar for tasks inferred from pane history |
| `learning.graduation_n` | 2 | consecutive confirmations needed to graduate (1-10) |
| `learning.confirmation_weight` | 3.0 | vote-weight multiplier for an operator confirmation in the confidence ratio (1 disables the boost) |
| `limits.max_consecutive_auto_prompts` | 10 | max consecutive auto-prompts per agent without human interaction |
| `limits.max_auto_prompts_per_minute` | 5 | rate limit per agent (rolling 1-minute window) |
| `limits.max_error_retries` | 2 | max retries per error signature |
| `safety.disable_never_auto_seed_patterns` | false | disable every shipped strict and heuristic never-auto rule |
| `llm.timeout_seconds` | 60 | timeout for LLM fallback calls |
| `llm.auto_act_confidence_threshold` | 999 (never) | min LLM self-reported confidence (0-100) to auto-act on a consult decision; below it (or no score) the situation escalates with reason `[llm_low_confidence]`. 999 is unreachable = never auto-act |
| `llm.pane_excerpt_chars` | 5000 | pane excerpt size in characters for LLM consult/rewrite context |
| `llm.rewrite_timeout_seconds` | 60 | timeout for rewrite calls |
| `llm.rewrite_fallback_template` | `You must act based on the following: {original_text}` | wraps original text when rewrite fails (placeholders: `{original_text}`, `{agent_name}`) |
| `llm.command` | (unset) | argv template for the consult fallback CLI (see llm fallback config) |
| `llm.command_start` | (unset) | argv template for the FIRST consult per agent; empty inherits `llm.command` (opt-in) |
| `llm.rewrite_command` | (unset) | argv template for the one-shot rewrite CLI (see llm rewrite) |
| `llm.rewrite_command_start` | (unset) | argv template for the FIRST rewrite per agent; empty inherits `llm.rewrite_command` (opt-in) |
| `llm.task_generate_command` | (unset) | argv template for the one-shot task suggestion given to an idle agent with NO task source, or a declared source that ran out (see task sources); empty keeps escalate-only behavior |
| `llm.task_generate_command_start` | (unset) | argv template for the FIRST task generation per agent (no-source case only; empty inherits `llm.task_generate_command`); an exhausted declared source only generates more tasks when BOTH this and `llm.task_generate_command` are set, and always uses `llm.task_generate_command` (never this) since a list already exists |
| `llm.task_generate_timeout_seconds` | 0 (inherits `timeout_seconds`) | timeout for one task-generation run |
| `embedding.disabled` | false | disable semantic matching entirely |
| `embedding.model_path` | (bundled all-minilm-l6-v2-q8_0.gguf) | path to a .gguf embedding model |
| `embedding.similarity_threshold` | 0.90 | min cosine similarity to reuse a learned signature |
| `embedding.bm25_min_score` | 0.35 | min normalized BM25 similarity for text fallback |
| `embedding.gpu_layers` | 0 | model layers offloaded to GPU (inert in official builds) |
| `embedding.model_context_window` | 0 (built-in default: 512 for MiniLM) | max tokens fed to the embedder before truncation; MUST NOT exceed what the model supports (over 512 hard-aborts a BERT/MiniLM native lib). raise only when `model_path` points at a larger-window model; values below 256 clamp up |
| `embedding.pane_salient_chars` | 800 | fallback signature window for idle/unclassified situations (trailing N chars) |
| `tui.max_content_width` | 0 (full width) | cap variable-width list columns; 0 = full width |
| `tui.theme` | default | TUI color theme: default, dark, light, high-contrast |
| `tui.terminal_bell` | true | ring the terminal bell (\a) on new escalations and on pauses caused by a different process |

TUI palette colors (`tui.palette.*`) are config.toml-only — roles: `title`, `section`, `error`, `ok`, `paused`, `running`, `warn`, `help`. values are 256-color codes (`"205"`) or hex (`"#ff5faf"`).

some settings are table-valued and live in `config.toml` only (not settable via `hap config set`): `[[capture_delay]]`, `[[task_sources]]`, `[[classifier]]`, and `[[safety.never_auto_rules]]`.

**capture delay** — the classification pane read waits a per-agent delay so the agent TUI has painted and event bursts coalesce. defaults: 10000ms (10s) on an agent's first event, 500ms after. override per agent type:

```toml
[[capture_delay]]
agent_type = "codex"   # or "*" for all
start_ms = 8000        # first-event delay
event_ms = 500         # subsequent-event delay
```

## safety rules (never-auto patterns)

list all rules (seed + custom):

```bash
hap rules list
```

add a custom regex pattern (operations matching this are never automated):

```bash
hap rules add '(?i)restart\s+the\s+payment\s+service'
```

remove a custom rule by index:

```bash
hap rules remove <index>
```

the 38 strict seed rules cover: force-push, `git reset --hard`, `rm -rf`, `sudo rm`, `DROP TABLE`, `TRUNCATE TABLE`, `DELETE FROM`, deploys to prod, `npm publish`, `terraform apply/destroy`, credential rotation, and more; broader heuristic seed rules catch suspected irreversible language. all shipped rules are active unless `safety.disable_never_auto_seed_patterns=true`. the old `safety.disable_seed` key still loads with a deprecation warning and is rewritten under the new name on the next config save.

the config key for custom patterns is `never_auto_patterns` (the old name `allowlist_patterns` still loads as a deprecated alias):

```toml
[safety]
never_auto_patterns = ['(?i)restart\s+the\s+payment\s+service']
```

prompts that look destructive but match no explicit pattern are caught by a suspected-irreversible heuristic. the heuristic requires corroboration — a destructive verb aimed at a data/infrastructure target, explicit no-undo language, etc. — so everyday prompts like "remove the unused import" don't trip it. it scans only the actionable region (the pending dialog or the next-task prompt), not the agent's narration. extend it with custom regex patterns:

```toml
# scoped to specific agent types
[[safety.never_auto_rules]]
pattern = '(?i)compact\s+the\s+conversation'
agent_types = ["codex", "agy"]   # "*" or omit for all agent types
```

the legacy `irreversible_indicators` and `[[safety.indicator_rules]]` settings still load with warnings and migrate to unified never-auto configuration on the next save.

## task sources

task sources point agents at a checklist file so idle agents get the next unchecked item.

list configured task sources:

```bash
hap task-source list
```

add a task source for a specific agent:

```bash
hap task-source add --agent backend-dev ./docs/backend-tasks.md
```

add a task source for a specific workspace (supports wildcards):

```bash
hap task-source add --workspace "codex-*" ./docs/tasks.md
```

add a task source for any agent in any workspace:

```bash
hap task-source add ./docs/tasks.md
```

add a task source with a custom prompt template:

```bash
hap task-source add --agent backend-dev --template 'Do this next: {next_task_content} (full list: {task_list_path})' ./docs/backend-tasks.md
```

remove a task source by index:

```bash
hap task-source remove <index>
```

this removes the `[[task_sources]]` entry only — the checklist file is left on
disk. it is unguarded and unconfirmed: it removes the entry even while a live
agent is mid-task on it, which makes it the force path.


### manage the task items (CRUD)

`hap task` edits the checklist items *inside* a source's file (whereas
`hap task-source` manages which file an agent points at). Address a list either
by the agent whose task source it is, or with `--path <file>` for any checklist
(and for workspace-scoped sources, which aren't addressable by agent name).

tasks are numbered by their **position in the file** (1..N, counting checked and
unchecked alike). the number never changes with a status filter — `done 3`
always means the third item in the file. numbers *do* shift after `add`/`remove`,
so every mutating command re-prints the renumbered list. the checkbox is shown
verbatim, so an in-progress `[-]` item (what the generated-task flow writes for
the task an agent is actively working) renders as `[-]` and is *not* counted as
pending (only truly-unchecked `[ ]` items are).

```bash
hap task backend-dev list                 # all items, with status + number
hap task backend-dev list --status pending  # or: done | all (default all)
hap task backend-dev get 3                 # show one item
hap task backend-dev add "wire up retries" # append a new unchecked item
hap task backend-dev done 2                # tick item 2 off ([x])
hap task backend-dev undone 2              # re-open item 2 ([ ])
hap task backend-dev update 2 "new text"   # edit text, keep status
hap task backend-dev remove 2              # delete item 2
hap task backend-dev send 3 [--yes]        # deliver pending item 3 to the live
#   agent NOW (y/N confirmation unless --yes). only a pending [ ] item on a
#   cleanly idle agent qualifies — idleness is re-checked at the moment of
#   delivery, so a stale --yes cannot interrupt an agent that has since picked
#   up work. the item is marked [-] BEFORE delivery (that mark is what stops
#   the daemon's idle-time flow re-sending it); a failed send returns it to [ ].

hap task --path ./docs/tasks.md list       # operate on any checklist file

# multi-line text stays ONE task: real line breaks (CR/LF flavors ok) are
# stored as the literal two-character sequence \n on the item's single line,
# and converted back to real newlines when the task is sent to an agent.
# hand-writing \n directly in tasks.md works the same way.
hap task backend-dev add $'wire up retries\nadd backoff jitter'   # 1 item: "wire up retries\nadd backoff jitter"
hap task backend-dev update 2 'part one\npart two'                # literal \n is kept as-is
```

resolution by agent name matches the `agent` selector a task source was
registered with (its short name, id, or type). if the name matches no source,
matches several, or only workspace-scoped sources exist, `hap task` errors and
tells you to use `--path`. writes go straight to the file (atomically) — the
daemon re-reads task files live, so no restart or reload is needed. adding a
task doesn't interrupt a working agent; it's picked up on the agent's next idle.

the TUI's **Tasks tab** does the same CRUD without the CLI: it aggregates every
configured source's checklist into one list (a header per source with a
tail-truncated path — `…/dir/file.md`, file name preserved, full path still
searchable — its items under it) and edits them in place — `enter`/`y` sends
the pending `[ ]` task under the cursor to the cleanly idle agent its source
feeds behind a Y/n confirmation, marking it `[-]` on success (done/in-progress
tasks and busy agents are refused), `v` opens a task
detail (full decoded text, full source path, live agents; `enter`/`y`, `e`,
`x`, `f` keep working inside it), `a` add, `e` edit, `d` done/undone, `x`
delete, `space` to mark a run so `d`/`x` act on all marked at once, `f` to
focus the live agent a source feeds, `/` to search. `x` **on a source's header
row** retires the whole source (config entry only, checklist file kept) behind
a y/n confirmation — offered only when no live agent matches it or every task
is finished, `[-]` counting as unfinished; an unknown agent list or an
unreadable checklist refuse too (unknown is not evidence of safety), and
marked items win over it. the add/edit prompts take
multi-line task text: **shift+enter inserts a line break** (ctrl+j on
terminals that can't report it), the box expands one line per break, **enter
submits** — stored as the literal `\n` encoding above, decoded back when the
prompt pre-fills. an action captured against a row aborts if that task's text
changed before the write lands, so a stale keypress never mutates the wrong
(renumbered) line.

### template placeholders

the prompt sent to the agent is rendered from a template. the default steers the
agent to manage its list through the `hap task` CLI with its own name pre-filled
in every command (and a `--path` fallback for sources that aren't name-addressable):

```
Your next task is {next_task_content}. Prefer the hap CLI to manage your tasks: `hap task {agent_name} list` to view them and `hap task {agent_name} done <n>` to mark one complete as you go (if that name isn't recognized, use `--path {task_list_path}` in place of `{agent_name}`).
```

- `{next_task_content}` — the text of the next unchecked item (or `"none"` when the list is complete)
- `{task_list_path}` — absolute path to the checklist file
- `{agent_name}` — the agent's hap-owned short name
- `{cwd}` — the agent's working directory (the project it is in)

when every item is checked off, the prompt is still sent with `{next_task_content} = "none"`, so the template can steer what an idle agent does when the list is done.

when an `[llm].command` is configured, each determined task is first reviewed by the llm before it is sent: via the `get_context`/`submit_decision` mcp tools it sees the live pane plus the queued task (`proposed_task`/`current_task`), the checklist path (`task_list_path`), and every remaining item (`pending_tasks`), then either sends the task as-is (`recommend_action` `@next_task:declared`, which sends the queued task verbatim), sends an edited task or a different pending item (literal `recommend_action` text), or declines (`@noop`). the outcome follows `auto_act_confidence_threshold` symmetrically — a confident review is applied automatically (the task is sent, or silently skipped on a decline), a low-confidence one is surfaced for confirmation (the suggestion is the llm's exact recommendation; the original task and reasoning show in the escalation detail). since the default threshold is 999, every review is surfaced until you lower it. this review is on by default; set `llm_review = false` on a `[[task_sources]]` entry (in `config.toml`) to opt that source out.

without a declared task source, the plugin falls back to inferring the next task from the agent's own native todo rendering (currently only `claude` agent type is supported for inference). other agent types skip inference and escalate.

if inference finds nothing (no task source and nothing inferable from the pane) and `llm.task_generate_command` is configured, the plugin runs that CLI once to synthesize a next task for the idle agent (placeholders: `{self}`, `{agent_name}`, `{agent_type}`, `{pane_excerpt}`, `{cwd}`). the CLI's stdout is surfaced as an escalation the operator confirms (writing a per-agent `tasks.md`) or dismisses — the plugin never sends a synthesized task unattended. leave `task_generate_command` unset to keep the default: an idle agent with no task source escalates as `no_task_source` and nothing is synthesized. `task_generate_command_start` is the first-interaction variant (empty inherits `task_generate_command`); `task_generate_timeout_seconds` bounds one run (0 inherits `llm.timeout_seconds`).

**`max_tasks` (per `[[task_sources]]`, default 20)** caps how large a source's checklist may grow. once the file holds MORE than `max_tasks` items — done, in-progress, and pending counted alike — and its pending items are exhausted, the daemon logs a warning (`maximum number of tasks reached … skipping task generation`, with the agent name) and skips LLM generation for that agent instead of appending to an already-long list. the **same cap gates manual creation**: adding tasks (the Tasks-tab `a`, or `hap task … add`) to a registered source is rejected once it would push the list past `max_tasks` (`maximum number of tasks reached …`), so a hand-added list can't grow past what the daemon would then refuse to refill. prune the checklist (or raise `max_tasks`) to resume. sending the pending items of an under-cap source is unaffected; the no-task-source bootstrap case (no `[[task_sources]]` entry) and an ad-hoc `--path` file that is not a registered source are never capped.

## reset data

### reset learned data (keeps config)

```bash
hap clear-data --yes
```

this empties all learning-related tables (signatures, decisions, audit log, corrections, rate/retry counters, LLM requests) and nudges the running daemon to reload — no restart needed. the `--yes` is mandatory. your configuration (thresholds, never-auto rules, task sources) is kept.

### full factory reset (everything, including config)

there's no single CLI verb for this — stop the daemon and delete the plugin's two directories:

```bash
pkill -f "hap daemon" 2>/dev/null                          # stop the daemon
rm -rf ~/.local/state/herdr/plugins/herd-auto-prompter     # DB, log, socket, lock
rm -rf ~/.config/herdr/plugins/config/herd-auto-prompter   # config.toml
```

both directories are recreated automatically — the daemon restarts on the next `pane.agent_detected`/`workspace.created` event, or immediately via `hap daemon --ensure`.

## llm fallback configuration

when no confident learned rule applies, the plugin can consult a local LLM. the model uses the plugin's MCP server (`hap mcp` — tools `get_context` and `submit_decision`). common CLI misconfigurations are auto-repaired at launch.

`get_context` returns: classified situation (type, options, permission verb, error summary), a pane excerpt (last `pane_excerpt_chars` characters), the agent's herdr location (`workspace_id`, `tab_id`, `pane_id`, `agent_id`), the agent's hap-owned short name (`agent_name`), and the pane's working directory (`cwd`, `foreground_cwd`). Whenever the agent has a matching `[[task_sources]]` entry, it also carries `task_list_path`, `pending_task_count` (items marked `[ ]`) with a truncated `next_pending_task`, and `in_progress_task_count` (items marked `[-]`, possibly the task the agent is currently working on) with a truncated `first_in_progress_task` — the preview field only appears when its count is at least 1 — on every consult, not just the pre-send task review below.

`submit_decision` enforces a per-situation contract:
- `approval`/`choice` with listed options → use `select_options` (1-based option numbers)
- `approval`/`choice` without listed options (e.g. bare y/n) → use `recommend_action` (literal text)
- `idle`/`error` → use `recommend_action` (literal reply text)
- any situation → `recommend_action "@noop"` means no reply is needed

### claude code

```toml
[llm]
command = [
  "claude", "-p",
  "Use the hap MCP tools: call get_context, decide what the operator would answer, then call submit_decision.",
  "--mcp-config", '{"mcpServers":{"hap":{"command":"{self}","args":["mcp"],"env":{"HAP_REQUEST_ID":"{request_id}"}}}}',
  "--allowedTools", "mcp__hap__get_context,mcp__hap__submit_decision",
]
timeout_seconds = 120
auto_act_confidence_threshold = 999   # 0-100; 999 = never auto-act (default). needs an LLM CLI that reports a confidence score
pane_excerpt_chars = 5000
```

### codex

codex requires `exec` for headless runs and explicit `HAP_DB_PATH`/`HAP_CONTROL_PATH` env vars (codex sanitizes the MCP server environment):

```toml
[llm]
command = [
  "codex", "exec", "--skip-git-repo-check",
  "--dangerously-bypass-approvals-and-sandbox",
  "-c", 'mcp_servers.hap.command="{self}"',
  "-c", 'mcp_servers.hap.args=["mcp"]',
  "-c", 'mcp_servers.hap.env.HAP_REQUEST_ID="{request_id}"',
  "-c", 'mcp_servers.hap.env.HAP_DB_PATH="{db}"',
  "-c", 'mcp_servers.hap.env.HAP_CONTROL_PATH="{control}"',
  "Use the hap MCP tools: call get_context, decide what the operator would answer, then call submit_decision. Do not run any other commands.",
]
timeout_seconds = 180
```

### antigravity (agy)

agy has no per-invocation MCP flag — register hap once in `~/.gemini/config/mcp_config.json`:

```json
{"mcpServers": {"hap": {"command": "/path/to/plugin/bin/hap", "args": ["mcp"],
  "env": {"HAP_DB_PATH": "~/.local/state/herdr/plugins/herd-auto-prompter/herd-auto-prompter.db"}}}}
```

then configure the llm command:

```toml
[llm]
command = [
  "agy", "--print",
  "Use the hap MCP tools: call get_context, decide what the operator would answer, then call submit_decision.",
  "--dangerously-skip-permissions",
]
timeout_seconds = 180
```

### placeholders

- `{self}` — path to the hap binary
- `{request_id}` — current pending request id
- `{db}` — path to the SQLite database
- `{control}` — path to the control socket
- `{agent_name}` — the agent's hap-owned short name (also usable in `llm.command` argv)

### first-interaction command variant

set `command_start` to use a different argv on the FIRST consult for a freshly detected agent (e.g. a heavier model or a warm-up prompt), then fall back to `command` for every consult after. leaving it empty inherits `command`, so it's opt-in and existing configs are unaffected. a `command_start` with no `command` does NOT enable the LLM — `command` alone gates that.

```toml
[llm]
command       = ["claude", "-p", "...", "--model", "haiku"]
command_start = ["claude", "-p", "...", "--model", "opus"]   # first consult per agent only
```

every LLM suggestion is re-gated through the never-auto patterns, kill switch, and rate guards. the LLM may act automatically only when its self-reported confidence score meets `auto_act_confidence_threshold` (0-100; default 999 = never) AND the action doesn't contradict learned history; below the threshold, with no reported score, or on timeout / no submission, the situation escalates. the old boolean `auto_act` still loads as a deprecated alias (`true` → threshold 0, `false` → 999) and is migrated on next save.

## llm rewrite (optional)

when a learned rule resolves to literal free text (idle next-task prompt, error retry command, free-text approval reply), the plugin can pass that text through a one-shot LLM CLI to adapt it to what's actually on the agent's screen before sending. unlike the consult fallback there is no MCP round-trip: the CLI is run once and its stdout is the rewritten text.

```toml
[llm]
rewrite_command = [
  "claude", "-p",
  "Rewrite this instruction for the coding agent given its current screen. Reply with ONLY the rewritten text.\n\nInstruction: {text}\n\nScreen:\n{pane_excerpt}",
  "--model", "haiku",
]
rewrite_timeout_seconds = 30
rewrite_fallback_template = "You must act based on the following: {original_text}"
```

### rewrite placeholders

- `{text}` — the literal reply a rule resolved to
- `{situation_type}` — the classified situation type
- `{agent_type}` — the agent type (claude, codex, agy, etc.)
- `{pane_excerpt}` — last `pane_excerpt_chars` characters of the live pane
- `{agent_name}` — the agent's hap-owned short name

also available as env vars: `HAP_REWRITE_TEXT`, `HAP_SITUATION_TYPE`, `HAP_AGENT_TYPE`, `HAP_AGENT_NAME`.

`rewrite_command_start` is the first-interaction variant of `rewrite_command` (same placeholders): used on the FIRST rewrite for a freshly detected agent, then `rewrite_command` for the rest. empty inherits `rewrite_command` (opt-in), and its "first" is tracked independently of `command_start`'s.

### rewrite invariants

- numbered-menu answers are never rewritten — a mapped digit reaches the menu untouched. only literal free text goes through the rewriter.
- a rewrite failure never blocks the send: on error, timeout, or empty output the original text is delivered wrapped in `rewrite_fallback_template`.
- safety controls still apply to the rewritten text: output matching the never-auto patterns or the irreversible heuristic is discarded in favor of the wrapped original.
- learning is unaffected: decision history records the original learned action, never the rewritten text.

## resolved paths (diagnostics)

print where hap keeps its config and state — useful when reporting an issue or jumping into the state dir. these resolve paths without creating anything, and work even before the files exist:

```bash
hap state-dir       # the state directory (DB, logs, socket, lock, match-index)
hap config path     # the config.toml path
hap paths           # both, labeled
```

example: `cd "$(hap state-dir)"` jumps into the state directory.

## version

```bash
hap version
```

## recipes

### quick setup for a new project

```bash
# check status
hap status

# rename agents to meaningful names
hap agents
hap rename brave-otter frontend-dev
hap rename cool-fox backend-dev

# point agents at task lists with custom templates
hap task-source add --agent frontend-dev ./docs/frontend-tasks.md
hap task-source add --agent backend-dev --template 'Do this next: {next_task_content} (full list: {task_list_path})' ./docs/backend-tasks.md

# lower the graduation bar during initial training
hap config set learning.graduation_n 1
```

### handle a batch of escalations

```bash
# see what's pending
hap escalations

# confirm the ones that look correct
hap confirm 1 --send
hap confirm 2 --send

# provide a different answer for one
hap resolve 3 --action "skip this test for now" --send

# mark a situation as needing no reply
hap resolve 4 --action @noop --send

# dismiss stale escalations without responding
hap dismiss 5 6 7

# prune all escalations older than 2 hours
hap escalations prune 120
```

### temporarily pause automation for risky work

```bash
# pause before doing dangerous ops
hap pause

# ... do your risky work ...

# resume when done
hap resume
```

### audit recent automation and correct a mistake

```bash
# review what happened
hap audit --limit 10

# correct a bad automated decision (demotes it back to shadow mode)
hap resolve <audit-id> --action "the correct response should have been X"
```

### inspect and prune learned signatures

```bash
# see what hap has learned
hap signatures list

# filter to only autonomous signatures with high confidence
hap signatures list --mode autonomous --min-conf 0.90

# inspect a specific signature
hap signatures show approval:9f2c

# delete one that's no longer relevant
hap signatures delete idle:3a1b --yes

# reset a graduated rule back to shadow (streak → 0; history kept)
hap signatures reset approval:9f2c --yes

# re-compute embeddings after switching embedding model
hap signatures reembed
```

## troubleshooting

- **escalations citing `not found in PATH`** — the daemon inherits herdr's environment, which can be narrower than your shell's. make sure the CLI is reachable from a non-login shell or use an absolute path in `llm.command`.
- **upgrades not taking effect** — the daemon is a singleton that outlives binary upgrades. since v0.1.13, `hap daemon --ensure` (fired by herdr's event hooks) detects the version mismatch and replaces the old daemon automatically. `hap status` shows the running daemon's version and flags a stale one. on older versions run `pkill -f 'hap daemon'` once after upgrading.

## notes

- `hap status`, `hap agents`, `hap escalations`, `hap audit`, `hap signatures list`, `hap signatures show`, `hap config show`, `hap config fields`, `hap config path`, `hap rules list`, `hap task-source list`, `hap task <agent> list`, `hap task <agent> get`, `hap kill-history`, `hap state-dir`, `hap paths`, and `hap version` are read-only and safe to run anytime.
- `hap confirm` and `hap resolve` with `--send` will deliver text to an agent pane — be mindful of what you send.
- `hap dismiss` drops escalations without responding — safe, nothing is sent or learned, audit rows kept as `dismissed`.
- `hap escalations prune [minutes]` bulk-dismisses old escalations (default 360 minutes).
- `hap signatures delete` erases the signature's decision history (audit rows are kept) — the plugin must re-learn that situation from scratch.
- `hap signatures reset` returns a signature to a fresh rule — shadow mode, streak → 0, confidence back to unscored (`conf=-`, since only post-reset decisions count and there are none yet) — while **keeping** its decision history (pre-reset decisions no longer count toward confidence/graduation, and the learned answer is still suggested). The only way to demote an autonomous rule (graduation is permanent; corrections no longer demote). It must re-earn `learning.graduation_n` confirmations to re-graduate.
- `hap signatures reembed` re-computes stored embeddings after an embedding model change; use `--force` to retry a previously failed pass.
- `hap pause` is the emergency kill switch — use it if automation is misbehaving.
- `hap clear-data --yes` is irreversible — it resets all learned patterns but keeps config.
- full factory reset requires deleting `~/.local/state/herdr/plugins/herd-auto-prompter` and `~/.config/herdr/plugins/config/herd-auto-prompter`.
- config edits apply live; no daemon restart needed.
- the daemon auto-starts via herdr plugin events (`pane.agent_detected`, `workspace.created`). you do not need to start it manually. `daemon --ensure` also auto-replaces a stale daemon left by an older binary.
- the MCP server (`hap mcp`) supports `HAP_DB_PATH` and `HAP_CONTROL_PATH` env vars for agent CLIs that sanitize the environment.
- `--workspace` in task-source supports name wildcards (e.g. `"codex-*"`, `"*-vscode3"`).
- `resolve --action @noop` (also: `noop`, `no_op`, `no-op`) means no reply was needed — nothing is ever sent to the pane.
- `hap tui` launches the interactive terminal UI (status, escalations, signatures, config) as an alternative to the read-only CLI commands. the escalation detail view offers per-item actions — confirm, resolve, dismiss, and **retry LLM** (re-invoke the local LLM on a consult that timed out or failed). retry is TUI-only; there is no CLI verb for it.
