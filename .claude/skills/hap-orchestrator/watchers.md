# watchers — monitors and crons that actually catch things

## the two that are always on

**The event stream**, as a persistent Monitor. Remember the last seq; restart it
after any daemon upgrade so it runs the new binary:

```sh
hap stream orchestrator --resume <last-seq>
```

**An hourly health check**, as a cron: `hap status` and `hap agents`; if paused,
only watch; start a dead daemon with `hap daemon --ensure`; read the screen of
anything stuck and unblock it; report only if something needed action.

## traps that made watchers lie

- **Do not key a main-branch CI watcher on "the latest run."** Version-bump
  commits produce `skipped` runs that arrive after the merge, so the watcher
  reports green about a run that tested nothing. Key on the merge commit:

  ```sh
  sha=$(gh pr view <n> --json mergeCommit --jq .mergeCommit.oid)
  gh run list --branch main --limit 12 --json headSha,status,conclusion \
    --jq ".[] | select(.headSha==\"$sha\")"
  ```

- **Do not use "number of completed tasks" as a progress proxy.** It cannot move
  during one long task, so it manufactures stalls. Key on whether hap has acted
  for that agent at all:

  ```sh
  lid=$(hap audit --limit 60 | grep -m1 "agent=<name>" | awk '{print $1}')
  # parked + unchanged last-audit-id across two polls = genuinely stuck
  ```

- **Scope to your own node.** `hap agents` lists other machines' agents; alerting
  on them is noise you cannot act on. Filter on the node column.

- **Silence is not success.** A watcher that greps only the happy path stays
  quiet through a crash. Include the failure signatures you would act on.

- **Retire a watcher when its subject is gone.** A lifecycle watcher on an exited
  agent emits errors forever.

## patterns worth reusing

```sh
# CI until final, reporting failures with their lines
while true; do
  out=$(gh pr checks <n> 2>/dev/null)
  printf '%s' "$out" | grep -qiE "pending|queued|in_progress" || { printf '%s' "$out" | grep -iE 'fail' || echo "all green"; break; }
  sleep 45
done

# a file appearing (an agent's report, a capture)
until [ -s /tmp/<report>.md ]; do sleep 20; done; echo "report written"
```

Prefer one watcher per question you would actually act on. Anything that fires
without changing what you do next is noise, and it competes with real signals.
