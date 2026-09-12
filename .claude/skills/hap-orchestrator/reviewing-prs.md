# reviewing an agent's PR

An agent's PR body is an argument, not evidence. Read it, then check the claims
against the diff.

```sh
gh pr view <n> --json title,body,additions,deletions,files
gh pr diff <n> | grep -nE "<symbol it claims to have added>"
gh pr checks <n>
```

## verifying, specifically

- **A reply on a review thread is not a fix.** Confirm the change is in the diff
  before treating a comment as addressed.
- **Agents push with the operator's credentials**, so "last comment author"
  cannot tell you whether the operator or an agent replied. Use the comment
  *count* on each thread.
- Unresolved threads across a PR:

  ```sh
  gh api graphql -f query='query($owner:String!,$repo:String!,$num:Int!){
    repository(owner:$owner,name:$repo){pullRequest(number:$num){
      reviewThreads(first:50){nodes{isResolved path line comments(first:20){totalCount}}}}}}' \
    -F owner=<owner> -F repo=<repo> -F num=<n> \
    --jq '.data.repository.pullRequest.reviewThreads.nodes[] | "\(.path):\(.line) resolved=\(.isResolved) n=\(.comments.totalCount)"'
  ```

- CI failures, with the failing test named:

  ```sh
  gh run view <run-id> --log-failed | grep -E "FAIL: |--- FAIL|\.go:[0-9]+:"
  ```

## regression or flake

Before accepting "it's flaky", require the same discipline you would want:
reproduce under the conditions CI used (`-race` if that is the failing job),
check the **same test on untouched main**, and then state which it is with the
evidence. Before accepting "it's a regression", check whether the failing test
even reaches the changed code.

A branch run and a main run can disagree; the merge commit's own build is the
authority.

## merging

Only merge when the operator has delegated it, and only when every review
comment is addressed and CI is green.

```sh
gh pr merge <n> --squash            # now
gh pr merge <n> --squash --auto     # when checks pass
```

**Auto-merge fires on *required* checks only.** If the test jobs are not
required, a PR can merge while they are still running — and then fail. After any
auto-merge, verify the merge commit's build (see [watchers.md](watchers.md)).

For stacked PRs, merge the base first; the child retargets to main by itself.
