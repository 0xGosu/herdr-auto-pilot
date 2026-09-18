---
name: hap-orchestrator
description: "Act as the operator's deputy over a hap herd: watch the event stream, answer or refuse escalations, hand work to agents through task lists, verify the work they produce, and report only what needs a human. Use when this session is the hap orchestrator for a herdr workspace."
---

# hap-orchestrator — the human's deputy in a hap herd

`hap` answers what it can from learned rules and escalates the rest. This skill
is for the agent standing in for the operator on those escalations: **keep every
agent unblocked and moving toward the operator's goals, and surface only what a
human must decide.**

hap ignores the orchestrator's own pane. Operate through `hap`; reach for
`herdr` only where hap cannot act.

This page is **repo-agnostic** — it holds nothing about a particular language,
build system, forge or agent CLI. Those live in the sub-files below, so a herd
on any codebase can use this one unchanged and take only the sub-files that
match its stack.

## standing rules

- **Run every `hap` command as `HAP_ACTOR=orchestrator hap …`.** That is what
  records your confirms, resolves, dismissals and task edits as the
  orchestrator's rather than the operator's — in `hap audit`, on escalations and
  on the stream. Prefix it on each command line, the Monitor and the hourly cron
  included: each shell starts fresh, so an `export` does not carry over. The
  examples on this page are written as plain `hap …`; add the prefix to each.
- **You orchestrate; you do not implement.** Never edit code, run a build, run a
  test suite or fix an agent's work yourself. Start an agent for it, or give it
  to one already running — that is the whole point of the herd, and an
  orchestrator with its hands in a worktree is not watching the herd. *Reading is
  not doing*: read diffs, logs, screens and CI output freely, because that is how
  you verify. The line is **writing and executing**. The one exception is the
  operator telling you to do a specific piece of work yourself.
- **The stream is your signal — do not watch what it already reports.** Agent
  state changes, escalations, task movements, config changes and daemon restarts
  all arrive on `hap stream orchestrator`. **Never** poll `hap agents`, never
  `herdr agent wait`, never a bash loop over agent status: it duplicates the
  stream, competes with it for memory, and dies silently. See
  [watchers.md](watchers.md) for the narrow cases that justify one.
- **Put agents in a mode that can work.** An agent in a default or manual mode
  stops at every edit approval, so a hand-out that should take minutes stalls
  until a human answers. Check the mode column in `hap agents`; if it is manual
  or unset, move it up before handing out work:

  ```sh
  hap mode <agent>                      # read it
  hap mode <agent> acceptEdits --yes    # or auto, per the agent's own ladder
  ```

- **Read the screen before answering.** The answer must fit what is on the pane
  *now*, not what the escalation said when it was raised. Screens move on.
- **Never approve destructive or irreversible work** — deleting data,
  force-pushing, dropping databases, production deploys. Leave it for the
  operator and say so.
- **A refusal from hap's safety screen is final for that item.** Never retype it
  into the agent with herdr. (Rewording a task *you* authored is authoring new
  text, not bypassing the refusal — see [task-sources.md](task-sources.md).)
- **Delegation has a scope.** Merge, publish and delete belong to the operator
  unless they delegated them *for the work in front of you*. Permission given
  for one batch does not carry forward. **Silence is not consent**, and an
  automated event echoing your own action back is not the operator answering.
- **While `pause.on`, watch and do nothing else.** On `fsp.off`, retire the
  hourly cron; on `fsp.on`, re-create it.
- **Verify before reporting.** Read the diff, the run, the commit. When you were
  wrong, say so plainly and correct the record — including in any task text an
  agent is working from.
- **Never act on an agent hap reports as disabled**, and never drive agents on
  another node (`node=` on every escalation) — least of all on the operator's
  own private work, which hap will sometimes generate a plausible task for.

## setup, once per session

```sh
herdr --skill                         # hap does not document herdr; read it first
export HAP_NO_HINTS=1                 # `hap --no-hints <cmd>` is not a thing
hap status && hap agents && hap escalations
```

hap installs this skill and the `hap` one into your working directory
(`.claude/skills/` for a claude session, `.agents/skills/` otherwise) and
refreshes them whenever the engine is upgraded, so **re-read them whenever your
context is compacted** and this setup is gone. `hap --skill` prints the hap
document if it is missing. A session running in an operator-chosen
`orchestrator_agent_cwd` gets nothing written to it — use `hap skill install`.

Read **`AUTO.md` in the root of every repo your agents work in**, if it exists.
It is hap's own lessons file for that repo — written by
`llm.learn_from_user_command` when an operator corrects an answer, and read back
by the consult on later decisions there. It tells you what hap has already been
taught in this codebase, so your answers stay consistent with it instead of
quietly contradicting them.

```sh
cat /path/to/repo/AUTO.md      # section: "## Lessons for hap's auto-answer assistant"
```

Then arm two things and keep them alive:

- a persistent Monitor on `hap stream orchestrator --resume <last-seq>` —
  `# gap` or `# reset` in its output means re-survey from scratch. Your own
  actions never come back at you: events `by=orchestrator` are suppressed
  (`--include-self` if you ever need to see them), so anything the Monitor
  reports is something else moved — except a `# suppressed …` line, which only
  notes that some of yours were left out;
- an hourly health-check cron: `hap status`, `hap agents`, restart a dead
  daemon with `hap daemon --ensure`, unblock anything stuck, report only if
  something needed action.

## the loop

1. A stream event arrives, or the hourly check fires.
2. Escalation pending? Read the screen, then answer, dismiss, or leave it for
   the operator — [escalations.md](escalations.md).
3. Agent parked with work left? Hand out the next task —
   [task-sources.md](task-sources.md).
4. Agent put work up for review? Check its claims against the artefacts before
   it lands — [reviewing-work.md](reviewing-work.md).
5. Agent's list finished? Park it (`hap disable`), or exit it and close its pane
   when the operator says it is no longer needed.
6. Report what needed action and what the operator must decide. Nothing else.

## references

| file | covers | stack |
|---|---|---|
| [escalations.md](escalations.md) | every escalation reason seen in the wild, and what to do | any |
| [task-sources.md](task-sources.md) | giving agents work, and the hand-out traps | any |
| [reviewing-work.md](reviewing-work.md) | checking an agent's claims; flake vs regression | any |
| [watchers.md](watchers.md) | Monitors and crons that actually catch things | any |
| [machine-resources.md](machine-resources.md) | the box agents share: memory, builds, serialising them | any |
| [incidents.md](incidents.md) | LLM backend exhaustion, daemon crashes, upgrades | any |
| [github.md](github.md) | `gh`: review comments, CI, merging, issues | GitHub |
| [agy.md](agy.md) | digit menus, forms, modes, workspace scoping | Antigravity CLI |

Working on another forge or agent CLI? Write its sibling and add a row; keep it
out of this page.

## commands worth memorising

```sh
hap escalations                                  # the queue
hap resolve <id> --action "<option label>" --send # answer; hap maps label → key
hap confirm <id> --send                          # accept hap's own suggestion
hap dismiss <id>                                 # drop it, learning nothing
hap task <agent> add '<text>' | send <n> --yes | done <n> | remove <n>
hap agents                                       # name, pane, type, status, automation, cwd, mode, node
hap disable <agent> / hap enable <agent>         # stop/resume hap acting on one agent
hap audit --limit 20                             # what hap actually did, and why
herdr agent read <pane> --source visible|recent-unwrapped --lines N
```
