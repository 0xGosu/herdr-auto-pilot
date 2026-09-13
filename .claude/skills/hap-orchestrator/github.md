# GitHub — the `gh` specifics

Everything here is GitHub-shaped. On another forge the *questions* still apply
(was the comment addressed, did the checks run, what is authoritative) but none
of these commands do. The generic discipline is in
[reviewing-work.md](reviewing-work.md).

```sh
gh pr view <n> --json title,body,additions,deletions,files,mergeStateStatus
gh pr diff <n> | grep -nE "<symbol it claims to have added>"
gh pr checks <n> --json name,state --jq '.[]|"\(.state)\t\(.name)"'
```

## review comments

Agents push with the operator's credentials, so **the comment author cannot tell
you who wrote it** — an agent's reply and the operator's original both read as
the operator. Count instead.

```sh
# line comments on the diff
gh api repos/<owner>/<repo>/pulls/<n>/comments \
  --jq '.[]|"\(.created_at|.[11:19]) \(.path):\(.line) — \(.body|.[0:90])"'

# comments raised on the PR itself, which have no diff thread to reply in
gh api repos/<owner>/<repo>/issues/<n>/comments --jq '.[]|.body|.[0:90]'

# did every thread get answered? originals + replies should reconcile
gh api repos/<owner>/<repo>/pulls/<n>/comments \
  --jq '[.[]|select(.body|test("^(Fixed|Addressed) in"))]|length'
```

A reply is not a fix. Confirm the change is in the diff before treating a thread
as addressed, and prefer replies that name the commit that fixed it.

Unresolved threads:

```sh
gh api graphql -f query='query($owner:String!,$repo:String!,$num:Int!){
  repository(owner:$owner,name:$repo){pullRequest(number:$num){
    reviewThreads(first:50){nodes{isResolved path line comments(first:20){totalCount}}}}}}' \
  -F owner=<owner> -F repo=<repo> -F num=<n> \
  --jq '.data.repository.pullRequest.reviewThreads.nodes[] | "\(.path):\(.line) resolved=\(.isResolved) n=\(.comments.totalCount)"'
```

## CI

```sh
gh pr checks <n>                                    # per-job state
gh run view <run-id> --log-failed | grep -aE "\-\-\- FAIL|^\s*FAIL|panic:|\.go:[0-9]+:"
gh run rerun <run-id> --failed                      # the flake/regression discriminator
```

**Find out which workflows actually run where before trusting any of them.**
This is not a detail — it decides what "green" means:

```sh
gh run list --branch <default-branch> --limit 10 --json name,conclusion
gh api repos/<owner>/<repo>/commits/<sha>/check-runs --jq '.check_runs[]|"\(.conclusion)\t\(.name)"'
```

A repo whose test workflow runs **on pull requests only** has no test build on
its default branch at all. Measured on this one: every merge commit carries
exactly one check, the release job — so "the merge commit's own build" is not an
authority, it is an empty set, and `gh run list --branch main` returns release
runs that tested nothing. In that shape **the PR run is the only test evidence
there will ever be**, so it has to be green before the merge, not after.

Jobs are not interchangeable either. Here `Tests (macos-14)` is the race-detector
job and `Tests (ubuntu-latest)` is not, so the same commit can pass one and fail
the other for reasons that have nothing to do with the change.

## merging

```sh
gh pr merge <n> --squash            # now
gh pr merge <n> --squash --auto     # when checks pass
```

**Auto-merge fires on *required* checks only.** If the test jobs are not
required, a PR can merge while they are still running, and then fail.

For stacked PRs, merge the base first; the child retargets by itself.

## issues

Raising one is the right move for a defect you found but were not asked to fix —
it outlives the session, unlike a line in a report.

```sh
gh issue create --repo <owner>/<repo> --title '<one line>' --body "$(cat <<'EOF'
<what was observed, with ids and timestamps, then the suggested fix>
EOF
)"
```
