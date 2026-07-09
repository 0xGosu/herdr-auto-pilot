# Herd Auto Prompter

**Keep your [Herdr](https://herdr.dev) coding agents unblocked, hands-free.**

Herd Auto Prompter is a Herdr plugin that watches every agent session in your
herd, detects when an agent needs input — finished a step, waiting on an
approval, stuck on a multiple-choice question, or stalled on an error — and
automatically supplies the next prompt or the correct response, *the way you
would*. It learns from your own past decisions in a supervised shadow mode,
acts autonomously only when its confidence clears your thresholds, and
escalates to you when it isn't sure. Everything it does is audited and
correctable.

- **Learned, not guessed** — every automated choice traces back to your own
  confirmed decisions.
- **Confidence-gated** — below the per-situation threshold it escalates, never
  guesses.
- **Safety first** — a never-auto allowlist (force-push, destructive ops,
  deploys, credential changes, …), a global pause/kill switch, a runaway-loop
  guard, and an error-retry ceiling all veto automation.
- **Fully local** — learning data, history, and the audit log live in SQLite
  on your machine. No telemetry, no cloud calls.

## Quickstart

Requires: Herdr ≥ 0.7.0 and `curl`. **No Go toolchain needed** — the install
step downloads the prebuilt binary for your platform (Linux/macOS,
amd64/arm64) from the matching GitHub Release and verifies it against the
published SHA256SUMS. (Building from source instead needs Go ≥ 1.24; see
Development.)

```sh
herdr plugin install 0xGosu/herdr-auto-pilot
```

Pin a release (recommended for reproducible installs), or install
non-interactively:

```sh
herdr plugin install 0xGosu/herdr-auto-pilot --ref v0.1.2
herdr plugin install 0xGosu/herdr-auto-pilot --yes
```

That's it. The monitoring daemon starts automatically when an agent appears in
the herd, and the **Auto Prompter** pane (TUI) is available from Herdr's pane
commands. Everything the TUI does is also a CLI verb on the same binary:

```sh
bin/hap status         # from the plugin dir, or put it on PATH
bin/hap escalations
bin/hap pause          # global kill switch
```

## How it learns (shadow mode)

The plugin never acts on a situation it hasn't learned from you.

1. **Observe.** When an agent needs input, the plugin classifies the
   situation (idle / approval / choice / error), fingerprints it into a
   *situation signature* (volatile stuff like paths, hashes, and timestamps
   is masked), and — in shadow mode — **escalates with a suggestion**.
2. **Confirm or correct.** In the TUI's *Escalations* tab press `enter` to
   confirm the suggestion (and send it), or `c` to type the correct
   response. From the CLI: `confirm <id> --send` or
   `resolve <id> --action TEXT --send`.
3. **Graduate.** After **5 consecutive consistent confirmations** (configurable)
   *and* confidence above the per-situation threshold, that signature becomes
   autonomous: next time, the plugin acts on its own and logs it.
4. **Stay in control.** Correct any automated decision post-hoc (TUI *Audit*
   tab or `resolve <audit-id> --action ...`). A correction demotes the
   signature back to shadow mode — it must re-earn your trust.

## Configuration

Config lives in the plugin config dir (`herdr plugin config-dir
herd-auto-prompter`) as hand-editable TOML; edits apply live (the daemon is
nudged, or picks them up on the next event).

```toml
[thresholds]
idle = 0.75
approval = 0.80
choice = 0.80
error = 0.85
inferred_task_bar = 0.90   # higher bar for tasks inferred from pane history

[learning]
graduation_n = 5           # consecutive confirmations to graduate

[limits]
max_consecutive_auto_prompts = 5   # per agent, without human interaction
max_auto_prompts_per_minute = 10   # per agent
max_error_retries = 2              # per error signature

# Point agents/workspaces at a task list so idle agents get the next
# unchecked item. Without a declared source, the plugin only infers a next
# task from an explicit checklist the agent itself printed — never free-form
# prose — and holds it to the higher inferred_task_bar.
[[task_sources]]
agent = ""            # agent id/name/type ("" = any)
workspace = ""        # workspace id ("" = any)
path = "/home/me/project/docs/tasks.md"
```

### Never-auto allowlist

Irreversible operations are **never** automated, regardless of confidence.
The shipped seed covers force-pushes, destructive filesystem/database ops,
deploys/publishes, credential changes, and more — and is regression-tested in
CI against a maintained corpus of irreversible-operation prompts
(`internal/domain/testdata/irreversible_corpus.txt`). Extend it with your own
regex patterns:

```toml
[safety]
allowlist_patterns = ['(?i)restart\s+the\s+payment\s+service']
```

or `hap rules add '<regex>'`. Prompts that *look* destructive
but match no pattern are escalated by a suspected-irreversible heuristic
rather than automated.

### Local LLM fallback (optional)

When no confident learned rule applies, the plugin can consult a local
LLM/agent CLI you already have installed. The model receives context and
submits its suggestion through the plugin's own MCP server
(`hap mcp` — tools `get_context` and `submit_decision`); its
stdout is captured for audit only. Example for Claude Code:

```toml
[llm]
command = [
  "claude", "-p",
  "--mcp-config", '{"mcpServers":{"hap":{"command":"{self}","args":["mcp"],"env":{"HAP_REQUEST_ID":"{request_id}"}}}}',
  "--allowedTools", "mcp__hap__get_context,mcp__hap__submit_decision",
  "Use the hap MCP tools: call get_context, decide what the operator would answer, then call submit_decision.",
]
timeout_seconds = 60
auto_act = false   # false: LLM suggestions are surfaced for your confirmation
```

Placeholders: `{self}` (this plugin binary), `{request_id}`, `{db}`,
`{control}`. Every LLM suggestion is re-gated through the same allowlist,
kill switch, and rate guards; with `auto_act = true` it may act only when it
doesn't contradict your learned history. On timeout or no submission the
situation escalates.

## Pause/kill switch & audit

- `pause` / `resume` (CLI, TUI `p`/`r`, or Herdr plugin actions) toggle a
  global kill switch. It takes effect within a second — the daemon re-reads
  the latest kill event on every decision — and the full pause/resume history
  is kept for audit.
- Every automated action **and** every escalation writes an audit record:
  trigger, situation, action or escalation reason, confidence, rationale, and
  (for LLM decisions) captured output. `audit` / the *Audit* tab shows it;
  corrections keep their lineage to the original decision.
- `clear-data --yes` resets all learned history and audit data (it never
  leaves your machine in the first place).

## Development

```sh
go build ./...        # build
go test ./...         # unit, golden, safety-invariant, concurrency, integration

# develop against your local checkout: linking skips the release-download
# build step, so build the binary yourself first
go build -o bin/hap ./cmd/hap
herdr plugin link .
```

See [CONTRIBUTING.md](CONTRIBUTING.md). The specification this plugin
implements lives in [`docs/specs/herd-auto-prompter/`](docs/specs/herd-auto-prompter/).

## License

[MIT](LICENSE)
