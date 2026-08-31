---
name: hap
description: "Control the Herd Auto Prompter (hap) plugin from Herdr. Check automation status, manage escalations, configure thresholds, manage agents, task sources, and safety rules — all via the hap CLI. Use when the user asks about auto-prompting, escalations, agent monitoring, or the hap plugin."
---

# hap — Herd Auto Prompter

`hap` is the CLI for the **Herd Auto Prompter** herdr plugin. A single daemon
watches every agent pane in the herd, classifies what each agent needs (idle,
approval, choice, error), and answers it from a rule learned from the
operator's own past decisions — escalating to the operator whenever it is not
confident or a safety control says a human must look.

## before anything else

```bash
hap status          # daemon health, paused?, pending escalations, agent count
```

If `hap` is not on `PATH`, it lives inside the plugin directory:

```bash
PLUGIN_ROOT=$(herdr plugin list --json | python3 -c 'import sys,json; print(next(p["plugin_root"] for p in json.load(sys.stdin)["result"]["plugins"] if p["plugin_id"]=="herd-auto-prompter"))')
HAP="$PLUGIN_ROOT/bin/hap"
```

## rules for an agent driving hap

These are the ones whose absence causes damage. Everything else in this
document is convenience.

- **Never type into an agent's pane to answer something hap escalated.**
  Respond through `hap confirm` / `hap resolve` / `hap dismiss`. Once a rule
  is autonomous the daemon answers matching prompts itself; a digit you also
  type races it — both keystrokes land, one picks the menu and the extra digit
  becomes stray text in the input box.
- **`resolve --action` takes the option's LABEL, not its digit.** hap maps the
  label to the keystroke the agent's TUI actually needs. A label matching no
  offered option is refused rather than sent, because a literal fall-through
  would commit option 1.
- **`--send` delivers to a live agent pane.** Without it nothing reaches the
  agent — the decision is only recorded and learned. Be sure of what you send.
- **`hap clear-data --yes` is irreversible** (all learned rules, decisions and
  audit rows). **`hap update` mutates the install** — never run it unprompted.
- **`@noop` means "no reply is needed"** and never sends anything to a pane.
- **Read-only, safe to run anytime:** `status`, `agents`, `audit`,
  `escalations` (bare), `kill-history`, `signatures list|show|search`,
  `config show|fields|path`, `config env list` (names only, never values),
  `config rules|task-source|classifier|capture-delay list`,
  `task <agent> list|get`, `state-dir`, `paths`, `version`, `gc --dry-run`.

## the CLI documents itself

Prefer these over guessing — they are always current, this file is a snapshot:

```bash
hap help                 # every command, grouped, plus common workflows
hap help task            # one command in full: usage, every flag, details, examples
hap escalations --help   # same page; --help/-h works anywhere in the arguments
hap config fields        # the authoritative key list `hap config set` takes
```

Most commands end with a **"Next steps"** footer with real ids filled in.
Suppress it when parsing output: `--no-hints` (one invocation), `HAP_NO_HINTS=1`
(one environment), or `hap config set cli.ai_agent_friendly_output false`
(persistent, default `true`). None of these affect help pages. Listings are
tab-separated; `hap state-dir` and `hap config path` always print a bare value.

## concepts

**situation types** — `idle` (finished, waiting for the next prompt),
`approval` (asking permission), `choice` (multiple-choice question), `error`
(hit an error, waiting for guidance). Structural forms herdr reports as idle —
Codex's Plan approval, Claude's "Select remote environment" picker — are
detected and classified as parked approvals.

**signature** — a fingerprint of a classified situation, with volatile data
(paths, hashes, numbers) masked. One learned rule per signature. Addressed by
unique prefix, git-style: `approval:9f2c`.

**shadow → autonomous** — a new signature starts in shadow: hap escalates with
a suggestion, you confirm or correct. After `learning.graduation_n` consistent
confirmations *and* confidence above the situation's threshold, it graduates
and hap acts on it unattended. A confirmation carries extra confidence weight
(`learning.confirmation_weight`, default 2×). **Graduation is permanent** — a
later correction is recorded and moves live confidence, but never demotes the
rule. `hap signatures reset` is the only way back to shadow.

**escalation** — hap is not confident enough (or a safety control refused), so
the situation is queued for a human instead of answered.

**never-auto patterns** — regexes that force a human decision whatever the
confidence says. hap ships a strict set (force-push, `sudo rm`, `dd`/`mkfs`,
`DROP TABLE`, prod deploys, `npm publish`, `terraform destroy`, cloud-resource
deletion, credential rotation, …) plus broader heuristic rules for suspected
irreversible language. `hap config rules list` prints the exact shipped set.
Routine locally recoverable work (recursive `rm` of a project dir, local git
resets, `terraform apply`, merging a PR) is deliberately **not** shipped — add
your own pattern for anything you want escalated anyway.

**semantic matching** — situations resolve to learned signatures by embedding
the masked salient content (bundled MiniLM via llama.cpp) and vector-searching
stored signatures, so a paraphrase reuses the rule. It degrades, never blocks:
normalized BM25 text matching takes over whenever the vector search does not
match (the embedder being down, *and* a search that ran but found nothing above
`similarity_threshold`), then exact hash.

**@noop** — the sentinel action meaning "no reply is needed". Recorded and
learned like any other decision; nothing is sent to the pane.

## how the daemon works

hap never attaches to an agent process. It observes and drives panes from the
outside, entirely through herdr's local socket + CLI, exactly as a human would.

**One long-lived daemon** monitors the whole herd. It is a singleton, started
and self-replaced on upgrade by herdr's `pane.agent_detected` /
`workspace.created` hooks running `hap daemon --ensure`. Every CLI command here
is a thin front-end talking to it over its control socket.

Per agent, the loop is:

1. **subscribe** — herdr's event stream reports when a pane needs attention.
2. **capture** — wait `[[capture_delay]]` (10s on an agent's first event, 2s
   after) so the agent TUI has painted and event bursts coalesce, then read the
   pane.
3. **classify** — into a situation type, extracting options, permission verb,
   or error summary.
4. **match** — mask volatile data, resolve to a learned signature (embedding →
   BM25 → exact hash).
5. **decide** — a confident, graduated rule that clears every safety gate is
   **acted** on. Otherwise **escalate** — or first **consult the LLM** if one is
   configured, whose suggestion is re-gated through the same controls.
6. **act** — a single-line reply is typed (`pane send-text` + `enter`) so a menu
   digit arrives as a keystroke; a multi-line reply is pasted as one message
   (`agent prompt`). A numbered menu is answered with its **digit**, mapped from
   the label. `@noop` records and learns but sends nothing.

Everything is out-of-process and fail-safe: LLM calls and deep pane reads run
in goroutines so the select loop keeps serving every agent, and any error
resolves to escalate + audit rather than a crash.

## agents

```bash
hap agents                        # tab-separated: name, pane id, type, status,
                                  # automation, cwd, permission mode
hap rename brave-otter backend-dev
hap disable backend-dev           # stop autonomous actions; it still escalates
hap enable backend-dev
hap capture backend-dev           # re-run the capture pipeline for one agent now
```

`cwd` and `mode` are `-` when unreadable. New columns are appended, so existing
field positions never move.

A disabled agent stays in the list marked `DISABLED`: autonomous actions are
audited as `denied` with `[agent_disabled]`, and escalations are audited
straight to `dismissed` and never enter the pending queue.

Use `hap capture` when an agent looks blocked but nothing appears in
`hap escalations`. It needs a running, current daemon.

### permission mode

The agent's own setting for how much it asks before acting — what `shift+tab`
cycles inside the agent's TUI.

```bash
hap mode backend-dev                  # print the mode alone, for scripts
hap mode backend-dev plan --yes       # rotate the agent into plan mode
```

| agent type | modes (in `shift+tab` cycle order) |
|---|---|
| `claude` | `acceptEdits`, `plan`, `auto`, `manual` |
| `codex` | `default`, `plan` |

Other agent types have no toggle and report `-`.

Setting works by pressing `shift+tab` and re-reading the pane until the agent
itself reports the target mode, so:

- It is **idempotent** — an agent already in that mode gets no keystroke and
  the command still succeeds. Safe to call unconditionally:
  `[ "$(hap mode backend-dev)" = plan ] || hap mode backend-dev plan --yes`
- It **fails rather than guesses.** The mode is read from the indicator the
  agent paints in its composer footer — nothing else reports it — so if an
  approval or form is covering that footer, both forms refuse. That refusal is
  a safety control: inside Claude's approval modals `shift+tab` is rebound to
  "approve with this feedback", so pressing it there would answer the prompt.
- **A mode the session does not offer** is detected, not forced. The cycle is
  per-session, not per-agent-type — a `--model haiku` claude rotates through
  only `manual`, `acceptEdits` and `plan`. hap notices when the rotation
  closes, rotates the agent **back to where it started**, and names the cycle
  it observed.
- **`bypassPermissions`** (`--dangerously-skip-permissions`) is reported but
  cannot be set — the cycle does not pass through it.
- `--yes` skips the y/N prompt and is **required** when not on a terminal.

## escalations

```bash
hap escalations                       # what needs a decision, with #ids
hap confirm <id> --send               # the suggestion is right — accept and deliver
hap resolve <id> --action TEXT --send # it is wrong — send the right answer instead
hap resolve <id> --action @noop       # no reply was needed (never sends)
hap dismiss <id> [<id>...]            # drop it; nothing sent or learned
hap escalations prune [minutes]       # bulk-dismiss everything older (default 360)
hap escalations retry <id>            # re-invoke the LLM on a failed/timed-out consult
```

- `confirm` is a **learning event**: enough consecutive confirmations graduate
  the rule. `resolve` records a **correction** — that is what hap learns
  instead.
- Without `--send`, the decision is recorded and learned but nothing reaches
  the agent, so a blocked pane stays blocked. That is how you accept work for a
  busy agent without interrupting it.
- `--send` requires a running daemon: the daemon types the reply, so it goes
  through the same never-auto screen and per-agent lifecycle barrier as its own
  sends. It is refused if the running daemon is older than the `hap` you
  invoked.
- `hap correct` is an alias for `hap resolve`.
- `retry` is queued — the daemon re-consults against the agent's **live** pane,
  so the answer reflects the screen now, not the one that failed.
- When the suggestion is generated TASKS, `confirm` appends them to the agent's
  task list and registers its source; `--send` also hands the first one over.

## pause / resume (kill switch)

```bash
hap pause          # global: stop all autonomous actions
hap resume
hap kill-history   # who paused/resumed (and full-self-prompting toggles), when
```

While paused, situations still classify and escalate — nothing is auto-answered
— and those escalations carry the rationale `[daemon_paused]`, meaning the
operator paused automation, not that anything crashed.

## audit

```bash
hap audit [--limit N]     # newest first, default 30
```

Columns: `#id`, time, status, situation type, action, confidence, LLM score,
rule mode, rationale. Every automated action and every escalation writes a row.
Correct a past decision with `hap resolve <audit-id> --action TEXT`.

Statuses worth knowing: `auto-sent` (a timed auto-accept answered it),
`fsp-sent` (full self-prompting answered it), `denied`, `dismissed`,
`reclaimed` (an unconfirmed task hand-out was returned to the queue).

## learned rules (signatures)

```bash
hap signatures list                          # alias: hap sigs
hap signatures list --type approval          # idle|approval|choice|error
hap signatures list --mode autonomous        # shadow|autonomous
hap signatures list --agent-type claude
hap signatures list --min-conf 0.85          # minimum live confidence

hap signatures search terraform              # keyword substring
hap signatures search "approve the file write" --semantic --limit 10 --min-score 0.5

hap signatures show approval:9f2c            # situation, recent decisions, last context
hap signatures reset approval:9f2c --yes     # back to shadow; history KEPT
hap signatures delete approval:9f2c --yes    # erase the rule and its decisions
hap signatures reembed [--force]             # after changing the embedding model
```

**`reset` vs `delete`.** `reset` returns the rule to shadow with streak 0 and
confidence unscored (it reads `conf=-`, not a number, because only post-reset
decisions count and there are none yet). All decision rows are kept and the
rule still suggests its learned answer, but pre-reset decisions no longer count
toward confidence or graduation — it must re-earn `learning.graduation_n`
confirmations. This is the only way to demote an autonomous rule. `delete`
erases the rule and its decision history entirely; audit rows are kept.

A `-` in any confidence field means "not scored yet", never a measured `0.00`.

## configuration

**Everything that writes `config.toml` is a `hap config` subcommand.** That is
the whole surface; the file never has to be opened by hand. (`hap task` is
deliberately not one — it edits checklist *items*, which is not configuration.)

```bash
hap config show                          # the effective config, defaults filled in
hap config fields                        # every settable key with its current value
hap config path                          # bare path, for scripting
hap config set <key> <value>
hap config set-threshold approval 0.80   # minimum|idle|approval|choice|error
```

Edits apply live — the command saves and nudges the running daemon to reload.
A hand-edited `config.toml` is **not** auto-detected (there is no file watcher);
it takes effect on the next CLI/TUI reload or daemon restart.

Array and map sections cannot be a `set` key (a list element is addressed by
position, a map entry by name), so each has its own topic — with its own guide
at `hap help config <topic>`:

| section | command |
|---|---|
| `[[task_sources]]` | `hap config task-source add / set / remove / list / provider` |
| `[[classifier]]` | `hap config classifier add / remove / list` |
| `[[capture_delay]]` | `hap config capture-delay set / remove / list` |
| `safety.never_auto_patterns` | `hap config rules add / remove` |
| `[[safety.never_auto_rules]]` | `hap config rules add --agent-type` / `remove-scoped` |
| `safety.disabled_seed_patterns` | `hap config rules disable-seed / enable-seed` |
| `[llm.*_env]` | `hap config env set / unset / list` (values read from stdin) |

These four were top-level verbs once (`hap rules …`). The old spellings still
resolve and print a migration note on **stderr**, so a script parsing their
tab-separated stdout is unaffected.

### every key, with its default

| key | default | what it does |
|---|---|---|
| `confidence_thresholds.minimum` | 0.50 | variance guard: minimum learned-action agreement |
| `confidence_thresholds.idle` | 0.65 | confidence needed to answer an idle agent |
| `confidence_thresholds.approval` | 0.70 | …an approval request |
| `confidence_thresholds.choice` | 0.70 | …a multiple-choice question |
| `confidence_thresholds.error` | 0.75 | …an error |
| `learning.graduation_n` | 1 | consecutive confirmations to graduate (1-10) |
| `learning.confirmation_weight` | 2.0 | vote weight of an operator confirmation (1 disables the boost) |
| `limits.max_consecutive_auto_prompts` | 30 | per agent, without human interaction |
| `limits.max_auto_prompts_per_minute` | 5 | per agent, rolling 1-minute window |
| `limits.max_error_retries` | 2 | per error signature |
| `escalations.auto_accept.enabled` | false | master switch for timed auto-accept |
| `escalations.auto_accept.approval` | `"5m"` | how long an approval escalation waits before hap answers it |
| `escalations.auto_accept.choice` | `"5m"` | …a choice |
| `escalations.auto_accept.error` | `"5m"` | …an error |
| `escalations.auto_accept.idle` | `"0"` | disabled — idle signatures are raw screen text, too weak to compare |
| `escalations.auto_accept.unclassifiable` | `"0"` | disabled, same reason |
| `full_self_prompting.enabled` | false | answer every escalation carrying a proposed answer, immediately |
| `full_self_prompting.honour_limits` | false | apply the `[limits]` ceilings to the mode, and switch it off when one is reached |
| `full_self_prompting.accept_generated_task` | false | also act on an idle escalation whose suggestion is an LLM-generated task |
| `safety.disable_never_auto_seed_patterns` | false | disable every shipped strict and heuristic rule |
| `llm.command` | (disabled) | argv for the consult CLI; this key alone gates the LLM fallback |
| `llm.timeout_seconds` | 60 | timeout for one consult |
| `llm.auto_act_confidence_threshold` | 85 | min LLM self-reported score (0-100) to auto-act; below it (or no score) escalates as `[llm_low_confidence]`. Set >100 (e.g. 999) to never auto-act |
| `llm.pane_excerpt_chars` | 5000 | pane excerpt size in the consult context |
| `llm.enable_rewrite_action` | false | let the consult LLM adapt learned free-text replies before delivery |
| `llm.run_in_agent_cwd` | true | run the CLI in the monitored agent's own project directory |
| `llm.rewrite_action_fallback_template` | `{original_text}` | optional wrapper when an action review fails (`{original_text}`, `{agent_name}`) |
| `llm.task_generate_command` | (disabled) | argv to synthesize a next task for an idle agent with no source |
| `llm.task_generate_timeout_seconds` | inherits `timeout_seconds` | timeout for one generation run |
| `llm.learn_from_user_command` | (disabled) | argv run when you CORRECT an escalation, to record the lesson in `AUTO.md` in the agent's project |
| `llm.learn_from_user_timeout_seconds` | inherits `timeout_seconds` | timeout for one learn run |
| `llm.env_file` | (none) | `.env` shared by every llm command |
| `llm.command_env_file` etc. | (none) | per-command `.env`, layered over the shared one (one per command) |
| `embedding.disabled` | false | turn semantic matching off entirely |
| `embedding.model_path` | bundled `all-minilm-l6-v2-q8_0.gguf` | path to a `.gguf` embedding model |
| `embedding.similarity_threshold` | 0.90 | min cosine similarity to reuse a learned rule |
| `embedding.bm25_min_score` | 0.35 | min normalized BM25 score for the text fallback |
| `embedding.bm25_highbar_score` | 0.70 | the stricter bar used instead, for a pane-tail salient that an embedding search RAN and refused. Can only tighten |
| `embedding.min_salient_chars` | 100 | below this (masked salient) a situation is matched by BM25 instead of embedding — see below |
| `embedding.pane_salient_chars` | 500 | signature window for idle/unclassified situations (trailing N chars) |
| `embedding.model_context_window` | 512 | max tokens fed to the embedder; MUST NOT exceed what the model supports |
| `embedding.embed_timeout_ms` | 2000 | stall guard per warm embed call |
| `embedding.warm_timeout_ms` | 30000 | stall guard for the first call, which loads the model |
| `logging.level` | info | `debug`/`info`/`warn`/`error`; read once at process start |
| `logging.max_size_mb` | 16 | log rotation size; read once at process start |
| `logging.audit_excerpt_retention_days` | 14 | see `hap gc` below |
| `tui.max_content_width` | 0 | cap variable-width list columns; 0 = full width |
| `tui.max_content_height` | 0 | cap the rows a list body may use; 0 = unlimited |
| `tui.theme` | default | `default`, `dark`, `light`, `high-contrast` |
| `tui.terminal_bell` | true | bell on a new escalation, and on a pause caused by another process |
| `tui.herdr_notification` | true | herdr desktop notification on those same two events |
| `tui.disable_check_for_update` | false | turn off the GitHub release check (TUI only, at most every 6h) |
| `tui.max_instances` | 1 | how many `hap tui` processes may run; starting one closes the oldest past this cap. `0` = no limit |
| `cli.ai_agent_friendly_output` | true | append the "Next steps" footer to command output |
| `task_source_provider.provider` | `local_fs` | default storage for every task list: `local_fs` or `github_gist` |
| `task_source_provider.env_file` | (none) | file holding `GITHUB_TOKEN` for `github_gist`; read at use time |
| `task_source_provider.timeout_seconds` | 20 | per remote store call |
| `task_source_provider.refresh_seconds` | 30 | how long a remote list is cached |
| `task_source_provider.github_gist.gist_id` | (none) | the gist hex id |
| `tui.palette.<role>` | theme default | `title`, `section`, `error`, `ok`, `paused`, `running`, `warn`, `help` |

Palette values are 256-color codes (`"205"`) or hex (`"#ff5faf"`); `""` clears
a role back to the theme. Anything else is rejected — lipgloss resolves an
unrecognized color to no color at all. These are hidden from the TUI config
tab, so `hap config fields` is where you read them.

**About `embedding.min_salient_chars`:** short text embeds indiscriminately, so
one almost-empty learned rule would otherwise match every unrelated screen. The
floor applies to both sides — a short situation is not embedded, a short rule
is stored without a vector and dropped from vector search — leaving such rules
reachable by BM25 and exact hash only. Structured salients (approval, choice,
error) are **exempt at any length**: they are short by construction
(`permission:proceed | options:no;yes` is 35 chars), so a floor over them would
switch off exactly the paraphrase matching the feature exists for.

### LLM commands: use the presets

The three `[llm]` command fields ship disabled and their argv is far too long
to retype. Bootstrap one:

```bash
hap config set llm.command --preset claude              # or: codex
hap config set llm.task_generate_command --preset claude
hap config set llm.learn_from_user_command --preset claude
```

A preset only ever bootstraps a field **nobody has configured** — once one is
set, tuning it is a `config.toml` edit (or the TUI Config tab's `e` on a
`(disabled)` row). `sample/config.toml` carries the full annotated argv.

### capture delay

How long to wait after a herdr event before reading the pane, so the agent TUI
has painted. Defaults: 10000ms on an agent's first event, 2000ms after.

```bash
hap config capture-delay list                # the delays in force, defaults resolved
hap config capture-delay set codex 8000 500  # <agent-type> <start-ms> <event-ms>; "*" = all
hap config capture-delay remove codex
```

Setting a type that already has a rule **overwrites** it — the daemon reads the
first matching rule, so a second one for the same type would never be reached.
A `0` means "keep the built-in default for that one".

### classifier rules

Which situation a pane is showing. Add your own when a screen is read as the
wrong situation, or as none at all (`unclassifiable` escalations). Operator
rules are consulted **before** the shipped ones, in the order added, so
position is precedence.

```bash
hap config classifier list
hap config classifier add --situation approval --agent-type claude --regex 'Do you want to proceed\?'
hap config classifier remove 0
```

`--situation` is `approval`, `choice`, `error` or `idle`; `--agent-type`
defaults to `*`. Repeat `--regex`/`--keyword` for several (a regex may contain
a comma, so they are never comma-split). A rule needs at least one of them.
Approval and choice rules only fire while herdr reports the agent blocked.

### LLM environment

Per scope: `shared`, `command`, `task_generate_command`,
`learn_from_user_command`.

```bash
hap config env list                                     # names only, never values
echo -n "$ANTHROPIC_API_KEY" | hap config env set command ANTHROPIC_API_KEY
hap config env unset command ANTHROPIC_API_KEY
```

No read path ever prints a **value**, and `env set` reads it from stdin unless
you pass `--value` — a token on argv lands in shell history and in every other
user's `ps`. To keep values out of `config.toml` entirely, point a scope at a
`.env` file instead (`hap config set llm.env_file <path>`).

Layering, last wins: daemon env → `env_file` → `env` → the command's
`…_env_file` → the command's `…_env`, with hap's own `HAP_*` injected last.
Names starting with `HAP_`/`HERDR_` are reserved and ignored. Env files are read
when the CLI is **spawned**, so edits apply with no restart. A configured file
that cannot be read, has a malformed line, or defines nothing **fails that run**
rather than launching the CLI without its credentials.

## safety rules (never-auto)

```bash
hap config rules list                    # shipped seed rules with stable ids, then yours
hap config rules add '(?i)restart\s+the\s+payment\s+service'
hap config rules remove <index>

hap config rules add --agent-type codex,agy '(?i)compact\s+the\s+conversation'
hap config rules remove-scoped <index>   # scoped rules have their own index space

hap config rules disable-seed <id>       # silence ONE shipped rule that over-escalates
hap config rules enable-seed <id>
```

A seed rule's `id` is a short hash of its pattern, so it names the same rule
across upgrades (and is rejected if that pattern no longer ships). One seed rule
is a single regex that may cover several phrasings — disabling it silences all
of them, not just the phrase you saw. To drop the whole shipped set instead, set
`safety.disable_never_auto_seed_patterns = true`.

Prompts that look destructive but match no explicit pattern are caught by a
**suspected-irreversible heuristic**. It requires corroboration — a destructive
verb aimed at a data/infrastructure target, explicit no-undo language — so
everyday prompts like "remove the unused import" do not trip it, and it scans
only the actionable region (the pending dialog or the next-task prompt), never
the agent's narration.

Deprecated but still loading, migrated on the next config save:
`allowlist_patterns` → `never_auto_patterns`, `safety.disable_seed` →
`safety.disable_never_auto_seed_patterns`, `irreversible_indicators` and
`[[safety.indicator_rules]]` → the unified never-auto forms.

## task sources

A task source points an agent at a checklist file so idle agents get the next
unchecked item. `hap config task-source` manages **which file**; `hap task`
manages the **items inside it**.

```bash
hap config task-source list                                   # every source, with its index
hap config task-source add --agent backend-dev ./docs/tasks.md
hap config task-source add --workspace "codex-*" ./docs/tasks.md   # "*" wildcards
hap config task-source add ./docs/tasks.md                    # any agent, any workspace
hap config task-source remove <index|agent>
```

Flags must come **before** the path — Go stops parsing flags at the first
positional argument (hap detects one written after and refuses rather than
silently ignoring it). Removal takes the config entry only; the checklist file
stays on disk. It is unguarded — it removes the entry even while a live agent
is mid-task, which makes it the force path.

**Every field is editable in place**, addressed by agent name or index:

```bash
hap config task-source set backend-dev path /new/tasks.md
hap config task-source set backend-dev agent swift-heron
hap config task-source set backend-dev workspace 'codex-*'
hap config task-source set backend-dev template 'Do: {next_task_content}'   # "" = default
hap config task-source set backend-dev auto-send-when-idle true
hap config task-source set backend-dev enable-llm-review-before-auto-send true
hap config task-source set backend-dev max-tasks 40
hap config task-source set backend-dev provider github_gist   # or "inherit"
hap config task-source set backend-dev gist-id aa11bb22       # or "inherit"
```

All of them can also be set at creation time (`--agent`, `--workspace`,
`--template`, `--auto-send-when-idle`, `--enable-llm-review-before-auto-send`,
`--max-tasks N`, `--provider`, `--gist-id`).

**Prefer the agent name over the index.** The index is positional, so removing a
source renumbers every one after it. `#0` (the spelling `list` prints) is
accepted verbatim. A name matching no source, or more than one, is refused
naming the indexes that disambiguate it; a workspace-scoped source has no agent
and must take an index. The first three re-point the source, so each prints what
it changed **from** — the next hand-out then comes from a different list, or goes
to a different agent. Nothing is copied or removed either way. A relative `path`
resolves against **your** shell's cwd (the daemon runs from the state dir).

### where task lists are stored

By default a checklist is a file on this machine. `[task_source_provider]` sets
the **default** storage; each source may override it, so some agents can be
local and others in a gist at once:

```bash
hap config set task_source_provider.provider github_gist
hap config set task_source_provider.github_gist.gist_id 3f2a1b9c4d5e6f708192a3b4c5d6e7f8
hap config set task_source_provider.env_file ~/.config/hap/task_source.env
hap config task-source provider           # what is in force (never prints the token)
```

A source that sets no `provider` keeps **inheriting** the default, so changing
the default moves it; hap never writes the inherited value into the entry.

Under `github_gist` a source's `path` is a file name **inside** the gist, not a
filesystem path. Set it and every matched agent shares one list; leave it out
and each matched agent gets its own `<agent-name>.md`, created on first
hand-out. hap never creates the gist — make a **secret** gist on github.com and
paste its hex id. The token lives in the file `env_file` names (`GITHUB_TOKEN=…`,
`gist` scope, mode 0600), is read at use time, and never enters `config.toml`.
Enabling this sends those sources' task lists to GitHub — task text and nothing
else.

`hap task <agent> …` works the same either way. `hap task --path` always reads a
**local** file, so address a remote list by agent name or source index.

### auto-send tasks to idle agents

By default a task goes out only when herdr reports the agent parked, and each
idle episode is driven once — an agent that finishes and just sits there waits.
Set `enable_auto_send_task_when_idle` and the daemon also polls every minute,
handing the next pending `[ ]` item to any matching agent idle for over a
minute:

```bash
hap config task-source add --agent backend-dev --auto-send-when-idle ./docs/tasks.md
hap config task-source set <index|agent> auto-send-when-idle true
```

- **The task is sent without waiting for anything to be learned.** Turning the
  flag on is your instruction, so a declared task from that source skips shadow
  mode and the idle confidence threshold, and a learned "do nothing" rule does
  not park it. That is what makes it work while you are away. Sources without
  the flag are unchanged.
- Every **safety** control still applies: kill switch, never-auto patterns, rate
  limits, per-agent disable, and the optional pre-delivery LLM review. Sends are
  audited under the trigger `auto-idle-send`.
- **A pending escalation does NOT stop the hand-out.** It is a question about
  what to answer on the agent's screen, not a verdict on whether the agent can
  take its next task, so queued work keeps flowing while you catch up.
- **One task, one agent.** Agents matched by the same source get *different*
  items, and the delivered item is marked `[-]` as it is sent. Reserving belongs
  to the source, so ordinary event-driven sends from it are marked `[-]` too.
- **Every sweep decides from current state, not from the last send.** A
  successful send only proves herdr took the keystrokes — text typed into a CLI
  that is restarting or unfocused is silently lost. So each hand-out is recorded
  and confirmed only when herdr reports that agent *working*. An unconfirmed
  hand-out whose agent is parked again after ~2 minutes is returned to `[ ]`
  (audit status `reclaimed`, trigger `auto-send-reclaim`) and re-offered in the
  same sweep. A `[-]` hap did not write itself is never touched.
- After **3** hand-outs of the same item that were never started, hap stops
  resending: the item is left `[-]` and escalated as `task_never_started`, and
  the agent moves on to the next item. Clear it with
  `hap task <agent> undone <n>`.
- An agent whose episodes keep resolving to something other than a send is
  re-checked on a **widening interval** (1, 2, 4 … capped at 15 minutes). Any
  delivered task resets it — a delay, never a bench.

### the prompt template

The default points the agent at its own list with its name pre-filled:

```
Your next task is {next_task_content}. Prefer the hap CLI to manage your tasks (start/done), run bash `hap task {agent_name} list` to view them (if that name isn't recognized, use the task-source index `{task_source_index}` in place of `{agent_name}`).
```

Placeholders: `{next_task_content}`, `{task_list_path}`,
`{task_list_path_quoted}` (that path as one shell word — use this inside a
command the agent runs), `{task_source_index}`, `{agent_name}`, `{cwd}`.

The lifecycle instructions (`start <n>`, `done <n>`, how `<n>` is addressed) are
printed by `hap task <agent> list` itself, beside the real task numbers, rather
than re-sent with every prompt.

**When every item is checked off the templated prompt is NOT sent.** hap
escalates a confirmable `@noop` suggestion ("No more pending tasks",
`task_source_exhausted`) instead, and never refills the list on its own —
rewriting a list the operator wrote is their call.

**`max_tasks` (per source, default 20)** caps how large a checklist may grow.
It gates **manual** creation — `hap task … add` and the TUI's `a` are rejected
once they would push the list past it — and confirming generated tasks whose
count would exceed it. Prune the list or raise `max_tasks` to resume. The
no-source bootstrap case and an ad-hoc `--path` file are never capped.

The cap also guards LLM generation for an exhausted source, but that guard is
currently dormant: refilling an exhausted source went away with
`llm.task_generate_command_start`, so nothing generates into a registered
source any more.

### task items (CRUD)

Address a list by the agent whose source it is, by the **source index** from
`hap config task-source list`, or with `--path <file>` for any local checklist.

**Addressing a task.** `list` numbers items by their position in the file
(`#1..#N`, counting checked and unchecked alike). A checklist may also number
its own tasks in the item text — the `1. `/`2. ` prefix hap's generated lists
use, or hand-authored ids like `3.4`. What you pass resolves like this:

| you pass | it means |
|---|---|
| `3.4` | the item whose id is `3.4` |
| `3` | the item whose id is `3` |
| `3` — no item has that id | position 3, **unless** the item there has an id of its own, which is refused so you spell it `#3` |
| `#3` | position 3, always |

The id comes first because it is what the agent reads in its prompt: told to do
"3.4 create the public link", it reports `done 3.4`, which must not tick off
whatever sits at position 3. Note `#3` needs quoting in a shell (`'#3'`).
Positions shift after `add`/`remove`, so every mutating command reprints the
renumbered list.

```bash
hap task backend-dev list                    # all items, with status + number
hap task backend-dev list --status pending   # or: done | all (default all)
hap task backend-dev get 3.4                 # show one item
hap task backend-dev add "wire up retries"   # append a new unchecked item
hap task backend-dev start 2                 # [-] in progress
hap task backend-dev done 2                  # [x]
hap task backend-dev undone 2                # [ ]
hap task backend-dev update 2 "new text"     # edit text, keep status
hap task backend-dev remove 2
hap task backend-dev move 5 2                # or: up | down (one step)
hap task backend-dev send 3 [--yes]          # deliver item 3 to the live agent NOW
hap task 0 list                              # by source index
hap task --path ./docs/tasks.md list         # any local checklist file
```

Aliases: `ls`, `show`, `create`, `wip`, `check`, `uncheck`/`reopen`, `edit`,
`rm`/`delete`, `mv`/`reorder`. Marks: `[ ]` pending, `[-]` in progress, `[x]`
done — an in-progress item is **not** counted as pending.

`move` reorders one task among its **siblings**; the source is a reference but
the destination is always a position. The whole subtree travels (nested detail
lines and sub-tasks). Moving a task under a different parent is re-parenting,
not reordering, and is refused.

`send` needs a pending `[ ]` item and a cleanly idle agent. Idleness is
re-checked at the moment of delivery, so a stale `--yes` cannot interrupt an
agent that has since picked up work. The item is marked `[-]` **before**
delivery (that mark is what stops the daemon re-sending it); a failed send
returns it to `[ ]`. Normally you do not need it — the daemon hands out the next
task by itself.

**Multi-line text stays ONE task.** Real line breaks are stored as the literal
two-character sequence `\n` on the item's single line and converted back to real
newlines when the task is sent. Hand-writing `\n` in the file works the same way.

```bash
hap task backend-dev add $'wire up retries\nadd backoff jitter'   # one item, two lines
```

Writes go straight to the file atomically; the daemon re-reads task files live,
so no restart or reload is needed. Adding a task never interrupts a working
agent — it is picked up on that agent's next idle.

## the LLM fallback (optional)

When no confident learned rule applies, hap can consult a local LLM/agent CLI.
The model talks to hap's own MCP server (`hap mcp` — tools `get_context` and
`submit_decision`); its stdout is captured for audit only. Configure it with a
preset (see above) rather than by hand.

`get_context` returns the classified situation (type, options, permission verb,
error summary), a pane excerpt (last `pane_excerpt_chars` chars), the agent's
herdr location (`workspace_id`, `tab_id`, `pane_id`, `agent_id`), its hap-owned
`agent_name`, and the pane's `cwd`/`foreground_cwd`. Whenever the agent has a
matching task source it also carries `task_list_path`, `pending_task_count` with
a truncated `next_pending_task`, and `in_progress_task_count` with a truncated
`first_in_progress_task` — on **every** consult, not just a task review.

`submit_decision` enforces a per-situation contract:

- `approval`/`choice` **with** listed options → `select_options` (1-based option
  numbers; one integer per tab for a multi-tab form)
- `approval`/`choice` **without** listed options (a bare y/n) → `recommend_action`
- `idle`/`error` → `recommend_action` (literal reply text)
- any situation → `recommend_action "@noop"` means no reply is needed

Every suggestion is re-gated through never-auto patterns, the kill switch and
the rate guards. It auto-acts only when the model's own `confident_score` meets
`llm.auto_act_confidence_threshold` **and** the action does not contradict
learned history; below that, with no score, or on timeout / no submission, the
situation escalates.

**Where the CLI runs.** By default (`llm.run_in_agent_cwd = true`) hap launches
it in the monitored agent's own working directory, so it reads that project's
`CLAUDE.md`/`AGENTS.md` and its `AUTO.md`. The directory is chosen by the
*agent*, which can `cd` anywhere — turn the key off where your agents work in
repos you do not trust, since a cloned repo can ship its own `AUTO.md`. (The
shipped prompts frame those rules as operator guidance, never as instructions
that override the prompt.) It cannot bypass a safety control either way. For
`claude`, hap appends
`--strict-mcp-config` to any command passing `--mcp-config`, so the project's
`.mcp.json` cannot add servers to the consult.

Placeholders for the argv template: `{self}` (the hap binary), `{request_id}`,
`{db}`, `{control}`, `{agent_name}`, `{agent_type}`, `{cwd}`, `{pane_excerpt}`,
`{session_id}`. Common misconfigurations of known CLIs are auto-repaired at
launch (claude/agy: prompt moved next to `-p`/`--print`; codex: missing `exec`
inserted).

For `agy` there is no preset and no per-invocation MCP flag — register hap once
in `~/.gemini/config/mcp_config.json` with `HAP_DB_PATH` in its `env`.

### LLM action review (optional)

When a learned rule resolves to literal free text (an idle next-task prompt, an
error retry command, a free-text approval reply), the consult LLM can adapt that
text to what is actually on screen before sending:

```bash
hap config set llm.enable_rewrite_action true    # default false; needs llm.command
```

`get_context` carries `proposed_action` (the exact text about to be sent), and
the model submits the adapted text, `@proposed_action:send` to affirm the
original verbatim, or `@noop` to send nothing.

Invariants:

- **Numbered-menu answers are never reviewed** — a mapped digit reaches the menu
  untouched. Only literal free text goes through.
- **Declared tasks are never reviewed here** — that is
  `enable_llm_review_before_auto_send`'s job, per source.
- **A review failure never blocks the send.** On error, timeout, empty or
  invalid output the original is delivered as-is (or wrapped in
  `rewrite_action_fallback_template`). `auto_act_confidence_threshold` does
  **not** apply — an unsure review degrades to the original instead of
  escalating.
- **Safety controls still apply to the reviewed text**: output matching a
  never-auto pattern or the irreversible heuristic is discarded in favor of the
  original, and the kill switch, rate guard and staleness re-check run again at
  delivery.
- **Learning is unaffected** — decision history records the original learned
  action, never the adapted text.
- **Cost:** every reviewed send is one full consult on `llm.command`.

### reviewing a task list before a task is sent (optional)

Per source, off by default. Immediately before the daemon auto-sends a task, the
LLM sees the live pane, the queued task (`proposed_task`/`current_task`), the
checklist path, and `tasks` — every item with the `ref` used to address it, its
position and status. It answers in **one** `submit_decision`: `task_actions` (an
ordered series of `done`/`delete`/`edit`/`move`/`add`, each addressing a task by
`ref`, with `add` taking an `as` handle) plus `send_task`, the **reference** of
the task to deliver once they are applied.

```bash
hap config task-source set <index|agent> enable-llm-review-before-auto-send true
```

`send_task` is never task text — the daemon renders the prompt from the list
itself, so nothing can be paraphrased in transit. To send the task at hand
unchanged, just name it and submit no actions. Everything applies atomically
under the file lock, or not at all.

**It never escalates.** An unusable review, one scoring below
`auto_act_confidence_threshold`, or a reviewed task tripping never-auto /
suspected-irreversible all send the **original** task unchanged and leave the
checklist byte-identical. `send_task "@noop"` is legal only when no pending task
remains afterwards. Every outcome is audited under the `llm-task-review` trigger
with a distinct reason, and mutations carry before/after text — so `hap audit`
answers "why is task 4 gone?". Only sends the **daemon** initiates are reviewed;
`hap task <agent> send` and the TUI's send never are.

### generating tasks when no source exists (optional)

If an idle agent has no matching source and nothing inferable from its own todo
widget (only `claude` supports inference), `llm.task_generate_command` runs a
one-shot CLI to propose next tasks. Its stdout is surfaced as an **escalation**
you confirm or dismiss — hap never sends a synthesized task unattended. Leave
the key unset and an idle agent with no source simply escalates as
`no_task_source`.

The CLI can decline with `@noop` alone, which escalates as a confirmable "do
nothing". An **empty** reply is not a decline — it is indistinguishable from a
crashed CLI, so it stays a retryable failure.

### learning from your corrections (optional)

When you **correct** an escalation, `llm.learn_from_user_command` runs a
one-shot CLI in the agent's own working directory, asking it to record the
lesson in `AUTO.md` there, under a heading spelled exactly
`## Lessons for hap's auto-answer assistant`. Extra placeholders:
`{situation_type}`, `{suggestion}` (what hap was about to answer),
`{correction}` (what you answered instead).

**`AUTO.md` is hap's own file, not the project's `CLAUDE.md`/`AGENTS.md`.** A
lesson only applies to the assistant hap spawns to answer a prompt on the
agent's screen; the shared memory file would load it into the agent's context on
every turn of its real work. So the three hap-spawned runs share a file nothing
else reads: this command writes it, and the shipped `llm.command` and
`llm.task_generate_command` prompts are told to read it before deciding (via the
`@AUTO.md` reference claude expands at prompt-parse time, so the consult needs no
Read tool). One heading keeps it reviewable and removable in one edit; add
`AUTO.md` to `.gitignore` if you would rather not commit it. Only the file NAME
lives in the prompt — no code depends on it.

- **Only corrections trigger it** — a confirmation means hap was right, so there
  is no lesson and no run. Accepting a generated task does not count either.
- **Only a standing escalation teaches.** Correcting an old **audit** row still
  feeds normal learning but runs no CLI: herdr recycles pane ids, so the lesson
  could land in a different project.
- **It runs in the agent's cwd**, and is **refused** rather than redirected when
  that cannot be resolved — the CLI has write permission and is told to edit
  "the current directory".
- It never touches the pane, never creates a rule, never escalates. Each run
  leaves one `hap audit` row (`llm-learn-from-user`: `learn:recorded` /
  `learn:failed`), retryable with `hap escalations retry <id>` or `l` in the
  TUI's Audit tab.
- **Nothing is parsed out of the reply** — no sentinel, no decision. Its whole
  stdout+stderr is captured verbatim on the audit row.
- It needs **write** permission (e.g. `--permission-mode acceptEdits`), runs
  only after your correction is committed, and is suppressed by `hap pause`.
  There is deliberately no `_start` variant.

## unattended modes

Two ways to stop an escalation queue from blocking the herd while nobody is
watching. Both are **off by default**, neither ever learns from its own accepts,
and both honour every safety exclusion.

### timed auto-accept — the slow lane

An escalation that has waited past its threshold, and whose situation is still
demonstrably on screen, is answered automatically.

```bash
hap config set escalations.auto_accept.enabled true
hap config set escalations.auto_accept.approval 15m     # "0" disables that type
```

Each duration is a Go duration string (`"15m"`, `"1h30m"`). A value below one
minute — the sweep's granularity — is **rejected at load**, as is anything
unparseable; the whole section is then ignored, so a typo can never start
sending on your behalf.

Before anything is delivered: the kill switch is off and the agent is neither
paused nor disabled; the agent still exists and is parked; and the pane still
shows **the same situation**, re-read and re-classified against the signature on
the audit row. If any of that cannot be *evaluated* (an unreadable pane, an
unreachable herdr), nothing happens and the escalation waits — only a check that
ran and came back negative retires one.

`idle` and `unclassifiable` ship disabled because their signatures fall back to
raw screen text, which cannot be compared confidently enough to deliver or
dismiss on.

What it never does: learn from itself; touch an escalation raised by
`never_auto_match`, `suspected_irreversible`, `retry_exhausted` or
`rate_limited` (all four are in code and cannot be configured away); type the
`@noop` sentinel at an agent; or answer more than **one escalation per agent per
sweep**. Escalations raised before you upgraded can never auto-accept — the
comparison needs a signature baseline older audit rows do not carry.

Outcomes: `auto-sent` for a delivered one; `dism:stale` / `dism:gone` /
`dism:failed` when hap retired one instead. None read as `resolved` — that stays
yours.

### full self-prompting — the fast lane

Every escalation carrying a proposed answer is accepted **immediately**, rather
than waiting out a threshold. Everything else about auto-accept holds unchanged.
It also refuses an agent that has **gone back to work** — status is re-read from
herdr immediately before delivery.

```bash
hap config set full_self_prompting.enabled true
```

**Enabling is refused until the daemon has earned it:** at least **10** graduated
(autonomous) rules AND a configured `llm.command`, and never while paused. The
error names every missing requirement at once. Turning it off is never refused.

The preconditions stay live. Delete rules below the minimum or clear
`llm.command` and the mode goes **inactive** — escalations queue for you again —
without your config being rewritten. `hap status` shows one of:

```
full self-prompting:           off
full self-prompting:           ON — escalations with a proposed answer are answered automatically
full self-prompting:           ON but INACTIVE — only 4 of 10 required graduated (autonomous) rules remain
```

An escalation the mode answered is audited as `fsp-sent` (a timed auto-accept
reads `auto-sent`), so the two are never confused.

Two opt-in behaviors, both default false:

```bash
hap config set full_self_prompting.honour_limits true
hap config set full_self_prompting.accept_generated_task true
```

**`honour_limits`** decides whether `[limits]` applies to the mode at all.

- **Left off (the default), the whole `[limits]` section is inert while the mode
  is active.** Neither runaway ceiling gates a send, a leftover runaway pause no
  longer benches the agent, and `max_error_retries` stops gating too — so a
  failing error signature retries without bound. This is blanket unattended
  autonomy. `hap config show` marks the `limits:` line as not enforced.
  Deliveries still *advance* the counters, so turning the key on after a long
  unattended run trips the consecutive ceiling on the very next sweep. Interact
  with the agent, or raise the ceiling, before flipping it.
- **Turned on, the ceilings are strict** — checked *before* each delivery, and
  reaching one switches the whole mode **off**: hap rewrites `enabled = false`,
  records it in `hap kill-history` (author `daemon`), and notifies which agent
  and which ceiling tripped. Note the asymmetry: the ceilings are **per agent**
  while the mode is **global**, so one runaway agent stands the mode down for
  the whole herd.

Everything outside `[limits]` is unaffected either way — the kill switch,
per-agent disables, never-auto rules and the irreversible heuristic all still
stop a send.

**`accept_generated_task`** lets the mode act on an idle escalation whose
suggestion is an LLM-generated task: writing the task list, registering the
source, handing the first task over. The generated text is screened against your
never-auto patterns and the irreversible heuristic first — it was authored by
the model *after* the escalation was raised, so until now your confirmation was
the only thing that had ever looked at it. A match leaves the escalation for
you. In practice it fires less often than you might expect: idle situations are
compared by raw screen text, and hap leaves anything it cannot prove.

## disk usage and cleanup

`hap status` prints a `disk:` line. Three things grow in the state dir:

| file | bounded by |
|---|---|
| `herd-auto-prompter.log` | `logging.max_size_mb` (16 MiB), plus one `.old` |
| `daemon.stderr.log` | 256 KiB, plus one `.old` |
| `herd-auto-prompter.db` | `logging.audit_excerpt_retention_days` (14) |

The database grows fastest — most of it is the pane excerpt captured with each
audit row (~3.8 KiB of a 5.0 KiB row). Retention **blanks that column and keeps
the row**, so `hap audit` history, rationales and statuses all survive.

```bash
hap gc --dry-run     # what would be reclaimed; changes nothing
hap gc               # reclaim now (the daemon also does this once a day)
hap gc --days 7      # override the window for this run
```

`audit_excerpt_retention_days` takes three kinds of value: omitted = the default
14; `0` = keep **no** excerpts (the most aggressive setting, not an off switch);
negative (`-1`) = never prune. Rows the daemon may still read are never touched
whatever the retention says — pending escalations at any age, rows with an
unprocessed LLM retry, and recently answered asks. That is a safety rule:
auto-accept reads a pending escalation's excerpt as proof a menu was standing.

`hap gc` also vacuums, which is what actually returns the space to the
filesystem.

## reset data

```bash
hap clear-data --yes    # every signature, decision, correction, counter, audit row
```

Config (thresholds, never-auto rules, task sources) is kept, and the running
daemon is nudged to reload — no restart. `--yes` is mandatory.

For a full factory reset there is no CLI verb — stop the daemon and delete both
directories (they are recreated automatically):

```bash
pkill -f "hap daemon" 2>/dev/null
rm -rf ~/.local/state/herdr/plugins/herd-auto-prompter     # DB, log, socket, lock
rm -rf ~/.config/herdr/plugins/config/herd-auto-prompter   # config.toml
```

## paths, version, upgrade

```bash
hap state-dir       # state dir (DB, logs, socket, lock, match-index) — bare value
hap config path     # the config.toml path — bare value
hap paths           # both, labeled
hap version
```

All resolve without creating anything and work before the files exist.

```bash
hap update            # install the newest release; MUTATES the install
hap update --force    # required on a `herdr plugin link` dev build
```

`hap update` runs `herdr plugin install 0xGosu/herdr-auto-pilot --yes`, then
prints the `daemon --ensure` follow-up that hands the running daemon over. **It
names an absolute path on purpose** — a plugin install does not repoint `hap` on
your `PATH`, so a bare `hap daemon --ensure` could restart the binary that was
just replaced.

## installing this skill for other agents

The skill ships **inside the binary**, so no checkout is needed:

```bash
hap skill                            # print it
hap skill install claude codex       # → ~/.claude/skills/hap/, ~/.codex/skills/hap/
hap skill install agents             # → ~/.agents/skills/hap/ (tools sharing ~/.agents)
```

The TUI's Config tab offers the same install.

## troubleshooting

- **An agent looks blocked but nothing is in `hap escalations`** —
  `hap capture <agent>` re-runs the classification pipeline now, as if herdr had
  raised an attention event. Then check `hap escalations` after a few seconds,
  or `hap audit --limit 10` to see the decision even if it did not escalate.
- **Escalations citing `not found in PATH`** — the daemon inherits herdr's
  environment, which can be narrower than your shell's. Use an absolute path in
  `llm.command`, or make the CLI reachable from a non-login shell.
- **Upgrades not taking effect** — the daemon is a singleton that outlives
  binary upgrades. `hap daemon --ensure` detects the version mismatch and
  replaces it; `hap status` flags a stale daemon (and one whose binary an
  upgrade removed, as `BINARY REMOVED`).
- **Every LLM consult comes back empty after an upgrade** — same cause: the
  daemon can no longer launch the MCP server from a binary that is gone.
  `hap daemon --ensure`.
- **The daemon is crash-looping or hung** — `hap status` exits non-zero and says
  which; `hap status --stderr` prints the captured daemon stderr tail.
- **Semantic matching degraded** — `hap status` reports the embedder's failure
  count, whether they were stall-guard timeouts, and the budgets in force. A
  model larger than the bundled MiniLM can exceed them on every call: raise
  `embedding.embed_timeout_ms` and `embedding.warm_timeout_ms`. Any `[embedding]`
  change rebuilds the embedder and clears the degraded latch, no restart needed.

## the TUI

`hap tui` (or the herdr pane) is a convenience, not a capability: every config
key and every action it offers is reachable from the CLI alone. Tabs: Status,
Agents, Tasks, Escalations, Audit, Rules, Config. `/` searches on most tabs, `v`
opens a detail view, `space` marks a run for batch actions. The escalation
detail offers confirm, resolve, dismiss, and **retry LLM** (the twin of
`hap escalations retry <id>`).
