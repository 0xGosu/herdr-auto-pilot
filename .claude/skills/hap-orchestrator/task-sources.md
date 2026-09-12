# task sources — giving an agent work

```sh
hap config task-source add --agent <name>          # one list per matched agent
hap config task-source set <idx|agent> path <list>.md
hap config task-source list
hap task <agent> add '<text>'
hap task <agent> send <n> --yes
hap task <agent> list | get <n> | done <n> | undone <n> | remove <n> | move <n> <pos>
```

## the traps, each learned the hard way

**A send is refused while the agent is working.** hap only delivers to a cleanly
parked agent. Wait for the park rather than retrying:

```sh
for i in $(seq 1 150); do
  st=$(hap agents | awk '$1=="<agent>"{print $4}')
  case "$st" in idle|done) hap task <agent> send <n> --yes && break;; esac
  sleep 15
done
```

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
with LLM-invented work. Check it before leaving a herd unattended.

## keeping a list honest

- Mark a task `done` when its deliverable exists, not when the agent says so —
  check the file, the PR, the CI.
- `remove` a task that events have made obsolete (a PR merged, a question
  answered) rather than leaving it to be handed out later.
- `update <n>` a pending task when you learn its premise was wrong; the agent
  reads the current text at hand-out time, so corrections land for free.
- Put a guard in the *text* when ordering matters ("if the PR is not merged yet,
  say so and wait") — it survives whatever order hap delivers in.
