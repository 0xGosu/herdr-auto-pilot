# escalations — triage and answering

Always `herdr agent read <pane>` first. An escalation records what the screen
said when it was raised; by the time you read it the agent may have moved on.

## the reasons, and what to do

| reason | means | do |
|---|---|---|
| `shadow_mode` | hap has a proposal but the rule has not graduated | `hap confirm <id> --send`, or `hap resolve … --action "<label>" --send` if the suggestion is wrong |
| `llm_no_submit … stale: situation changed during consult` | the screen moved while the LLM thought | usually a ghost — `hap dismiss`. Frequent with fast-repainting agents |
| `llm_timeout` | consult exceeded `llm.timeout_seconds` | answer it; if it recurs, raise the budget (`hap config set llm.timeout_seconds 120`) |
| `llm CLI failed without submit_decision` | the LLM backend itself is broken | [incidents.md](incidents.md) — do not treat as a one-off |
| `never_auto_match` | a safety rule forced a human | operator-only. Do not bypass, do not rephrase into the agent |
| `suspected_irreversible` | heuristic matched the pane | read the screen: it often matched the agent's own prose |
| `no_task_source` | the agent has no list | register a source, or dismiss if it is not your node |
| `task_source_exhausted`, `noop_vs_pending_tasks` | bookkeeping notices about a queue | dismiss; latched once per parked episode on current builds |
| `unclassifiable` | hap cannot read the screen | often a vendor form — [agy.md](agy.md) |

## answering

Pass the option **label**; hap maps it to the key the TUI needs.

```sh
hap resolve 224349670177267712 --action "Yes, run command" --send
hap resolve <id> --action @noop        # teaches "no reply was needed"
```

**Prefer the narrowest option.** On a command approval, "Yes, run command"
keeps every later command going through hap's safety screen; "always allow
commands starting with X" pre-allows a prefix, so a dangerous command later
never raises a prompt at all and never reaches the never-auto rules.

`dismiss` drops a row without learning; `resolve` teaches. Choose deliberately —
dismissing a genuine correction throws away the lesson, and resolving a ghost
teaches hap about a screen that no longer exists.

## your corrections become lessons

`hap resolve` does more than answer: `llm.learn_from_user_command` may record a
short rule in that repo's `AUTO.md`, under the heading
`## Lessons for hap's auto-answer assistant`, and the consult reads it back on
later decisions for the same repo. So read `AUTO.md` at the start of a session
and again after a correction that mattered — it shows which of your decisions
stuck, and a lesson that came out wrong is worth editing rather than leaving to
steer every future answer.

Two caveats: the file lives in the **agent's** repo, not hap's state directory,
so each repo has its own; and with some backends the loop is one-way — lessons
are written but never read back — so check rather than assume.

## clearing inert notices in batches

Queue notices (`task_source_exhausted`, `noop_vs_pending_tasks`) are inert —
hap sends nothing while they sit. Clear them in a sweep rather than reacting to
each one:

```sh
for id in $(hap escalations | awk '$3=="idle"{print $1}' | tr -d '#'); do
  hap dismiss "$id" >/dev/null && echo "cleared $id"
done
```

Keep `approval` rows out of such sweeps — read those individually.

## what not to do

- Do not answer an approval you have not read on screen.
- Do not widen a permission on the operator's behalf ("always allow", "persist
  to settings") — that is theirs to decide.
- Do not add a never-auto rule whose pattern appears in *every* prompt of that
  type: it escalates all of them and the agent stops. Match the **answer**, not
  the screen (`hap config rules add --action …` where available).
