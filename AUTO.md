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
