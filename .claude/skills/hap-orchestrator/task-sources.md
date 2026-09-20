# task sources — giving an agent work

```sh
hap config task-source add --agent <name>          # one list per matched agent
hap config task-source set <idx|agent> path <list>.md
hap config task-source list
hap task <agent> add '<text>'
hap task <agent> send <n> --yes
hap task <agent> list | get <n> | done <n> | undone <n> | remove <n> | move <n> <pos>
```

## before the first hand-out: check the mode

An agent in a default or manual mode stops for approval on every file edit, so
work you hand it stalls behind a prompt nobody is watching. The mode column in
`hap agents` shows it; move it up before you queue anything.

```sh
hap mode <agent>                      # read it
hap mode <agent> acceptEdits --yes    # idempotent; or auto, per the agent's ladder
```

Prefer the most autonomous mode the agent offers and the operator allows. `hap
mode` refuses while a modal covers the composer footer — clear the modal first.

## the traps, each learned the hard way

**A send is refused while the agent is working.** hap only delivers to a cleanly
parked agent. Wait for it to park — the stream reports the transition, so watch
for the event rather than polling `hap agents`:

```sh
hap task <agent> send <n> --yes    # once the stream has reported it parked
```

Better still, let hap decide when: a source with `--auto-send-when-idle` hands
the next task over the moment the agent parks, with no watching at all. That is
the shape to reach for — the orchestrator should not be sitting on a timer
waiting for an agent when the daemon is already doing exactly that.

**hap marks the sent task `[-]` itself.** Do not tell agents to run
`hap task … start <n>` — a hand-out never says which number it carried, so the
agent guesses. One reopened already-merged work that way. Say instead: *hap has
already marked this in progress; use `list` to see numbering and `done <n>` when
finished.*

**Task text is screened by hap's safety rules.** Prose describing code can trip
the pane heuristics. Real refusals: a sentence containing "delete … production",
the word "irreversible" while describing that very rule, and "forced off …
push". Rewording your own task is authoring new text — legitimate; retyping the
same text past a refusal is not.

**Keep digits out of the hand-out template for agy** — a digit can commit a menu
choice. Set a template without `{task_source_index}`:

```sh
hap config task-source set <idx> template 'Your next task is {next_task_content} -- hap has already marked this task in progress; run `hap task {agent_name} list` for numbering and `hap task {agent_name} done <n>` when finished.'
```

**Never set `--auto-send-when-idle` for an agy source.** agy parks at its
approval menu, hap reads that as idle, and the hand-out is typed into the open
menu.

**`accept_generated_task=false`** keeps an exhausted list from being refilled
with LLM-invented work. Check it before leaving a herd unattended — the problem
is not that generated tasks propose merging a change, deleting its branch and
removing the worktree (those are finishing steps, and yours to take when the
work in front of you calls for them), but that the generator invents work
NOBODY ASKED FOR and has named the wrong agent while doing it.

## prompting an agent directly

Sometimes the task list cannot say it — a coordination instruction, a
correction. Then, and only then, prompt the pane. Two things bite:

**A prompt to a busy agent is queued, not delivered.** It lands when the current
turn ends. So a correction arrives alongside the thing it corrects, and must
supersede it in its own words — *disregard my previous message* — rather than
assuming the agent sees them in order.

**Verify it landed.** Read the pane back. A queued message shows in the composer
with a `Press up to edit queued messages` hint; nothing at all means it never
arrived. Never assume delivery from the command's exit status alone.

## keeping a list honest

- Mark a task `done` when its deliverable exists, not when the agent says so —
  check the file, the PR, the CI.
- `remove` a task that events have made obsolete (a PR merged, a question
  answered) rather than leaving it to be handed out later.
- `update <n>` a pending task when you learn its premise was wrong; the agent
  reads the current text at hand-out time, so corrections land for free.
- Put a guard in the *text* when ordering matters ("if the PR is not merged yet,
  say so and wait") — it survives whatever order hap delivers in.
