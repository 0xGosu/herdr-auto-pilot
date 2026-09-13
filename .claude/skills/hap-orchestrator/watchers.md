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

- **Find out which builds actually run before keying a watcher on one.** A
  watcher aimed at a build that does not exist for that ref reports whatever
  unrelated job it happens to match. Some repos run their test workflow on
  proposed changes only, so the default branch has no test build at all and
  "the merge commit's own build" is an empty set. Check first, then key on
  something real — [github.md](github.md) for the `gh` form.

- **Do not use "number of completed tasks" as a progress proxy.** It cannot move
  during one long task, so it manufactures stalls. Key on whether hap has acted
  for that agent at all:

  ```sh
  lid=$(hap audit --limit 60 | grep -m1 "agent=<name>" | awk '{print $1}')
  # parked + unchanged last-audit-id across two polls = genuinely stuck
  ```

- **A waiting agent looks exactly like a wedged one.** An agent blocked on a
  fifteen-minute build or a CI run is parked with no hap activity, which is the
  same signature as hung. Before acting on a stall alert, check whether the
  thing it is waiting for is alive — a running child process, a job still in
  flight. Every stall alert in one session was a false positive, and false
  alerts are how you learn to ignore the real one.

- **Scope to your own node.** `hap agents` lists other machines' agents; alerting
  on them is noise you cannot act on. Filter on the node column.

- **Silence is not success.** A watcher that greps only the happy path stays
  quiet through a crash. Include the failure signatures you would act on.

- **Retire a watcher when its subject is gone.** A lifecycle watcher on an exited
  agent emits errors forever.

## watchers die, quietly

Background watchers are the first thing killed under memory pressure — seven
went in one session while builds ran, each without a sound until its output
never arrived. A watcher you believe is armed may not be.

So: while a build or any memory-hungry job is running, **do not re-arm
watchers**. Take one-shot readings instead, and lean on the event stream, which
runs in its own process and survives. See
[machine-resources.md](machine-resources.md).

## patterns worth reusing

```sh
# poll until a job reaches a terminal state, then report which
for i in $(seq 1 25); do
  s=$(<status command> 2>/dev/null)
  case "$s" in *pending*|*running*|*queued*) sleep 90;; *) echo "$s"; break;; esac
done

# a file appearing (an agent's report, a capture)
until [ -s /tmp/<report>.md ]; do sleep 20; done; echo "report written"
```

Prefer one watcher per question you would actually act on. Anything that fires
without changing what you do next is noise, and it competes with real signals.
