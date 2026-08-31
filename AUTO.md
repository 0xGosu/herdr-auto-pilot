# AUTO.md

hap's own file. Nothing here is loaded into an agent's ordinary context — it is
read only by the runs hap spawns (`llm.command`, `llm.task_generate_command`) and
written by `llm.learn_from_user_command`.

## Lessons for hap's auto-answer assistant

- **Finishing a change means MERGING it.** A green PR is not a delivered PR: once CI is
  passing and every review comment is answered, squash-merge it yourself
  (`gh pr merge --squash`, keeping `[skip ci]`-family markers out of the message), then
  remove the worktree and delete the branch. Don't park a ready branch and wait to be told
  — hold only when CI is red or still running, a review is requesting changes, or the merge
  turns on a call the operator has not already made.
