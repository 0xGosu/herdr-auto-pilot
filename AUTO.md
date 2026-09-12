# AUTO.md

hap's own file. Nothing here is loaded into an agent's ordinary context — it is
read only by the runs hap spawns (`llm.command`, `llm.task_generate_command`) and
written by `llm.learn_from_user_command`.

## Lessons for hap's auto-answer assistant

- **Finishing a change means MERGING it.** A green PR is not a delivered PR: after pushing,
  follow CI to a verdict yourself and fix what it reports — failing checks, bot and human
  review comments — then squash-merge (`gh pr merge --squash`, keeping `[skip ci]`-family
  markers out of the message), remove the worktree and delete the branch. CI still running
  or red is never a reason to hand the branch back; hold only when a review or the merge
  turns on a call the operator has not already made.
- Approve a temporary live daemon swap to measure a fix when the herd and orchestrator stay running and restoration is planned; choose “Yes, swap and measure.”
- **Approve a permission prompt for THIS command only — pick the plain “Yes, run command”.** The
  “always allow …” options widen a one-off approval into a standing rule the operator never asked
  for, and the prefix they name is the one the tool chose (often a different path or subcommand
  than the command on screen), so accepting one both grants too much and grants the wrong thing.
  Read-only commands are no exception; only the operator broadens a permission.
- **Never answer a vendor's feedback, survey or telemetry prompt on the operator's behalf —
  take Skip.** A rating is their opinion, not an inference about the work, and the screen
  interrupts the task rather than belonging to it. Choose by LABEL (Skip, Dismiss, Not now) —
  the digit differs per screen, and an unmatched reply commits the first option.
