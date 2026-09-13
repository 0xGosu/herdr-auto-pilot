# watchers — and why you want almost none

## the stream already tells you

`hap stream orchestrator` reports every hap event: escalations raised and
answered, task items handed out and completed, task sources added, config
changes, pause and FSP toggles, daemon restarts. **An agent changing state is a
stream event.** So is the thing you were about to poll for.

A watcher built on top of that is pure duplication. It costs memory, it competes
with the stream, and — the part that bites — **it dies silently under memory
pressure**, so you carry on believing you are watching something you are not.
Seven died unnoticed in a single session while builds ran.

**Never** do these:

```sh
herdr agent wait <pane> ...                        # the stream says it
while ...; do hap agents | grep <name>; sleep; done # the stream says it
while ...; do hap escalations; sleep; done          # the stream says it
```

## the two that are always on

**The event stream**, as a persistent Monitor. Remember the last seq; restart it
after any daemon upgrade so it runs the new binary:

```sh
hap stream orchestrator --resume <last-seq>
```

**An hourly health check**, as a cron: `hap status` and `hap agents`; if paused,
only watch; start a dead daemon with `hap daemon --ensure`; read the screen of
anything stuck and unblock it; report only if something needed action. Retire it
on `fsp.off`; re-create it on `fsp.on`.

That is the standing set. Adding a third is the exception, not the routine.

## when a watcher is actually justified

Only for something **outside hap**, which therefore emits no stream event — a CI
run on a forge, a release publishing. Even then:

- **Prefer a one-shot check to an armed watcher.** You usually want the answer
  once, at a moment you choose, not a subscription.
- **One watcher per question you would act on.** Anything that fires without
  changing your next move is noise competing with real signals.
- **Do not arm one while a build is running** — that is exactly when it will be
  killed. Take readings by hand until the box is quiet.
- **Retire it the moment it answers**, and whenever its subject is gone. A
  lifecycle watcher on an exited agent emits errors forever.

## traps that made watchers lie

- **Find out which builds actually run before keying a watcher on one.** A
  watcher aimed at a build that does not exist for that ref reports whatever
  unrelated job it happens to match. Some repos run their test workflow on
  proposed changes only, so the default branch has no test build at all and
  "the merge commit's own build" is an empty set. Check first, then key on
  something real — [github.md](github.md) for the `gh` form.

- **A waiting agent looks exactly like a wedged one.** An agent blocked on a
  fifteen-minute build or a CI run is parked with no hap activity, which is the
  same signature as hung. Before acting on a stall alert, check whether the thing
  it waits for is alive. Every stall alert in one session was a false positive,
  and false alerts are how you learn to ignore the real one.

- **Scope to your own node.** `hap agents` lists other machines' agents; alerting
  on them is noise you cannot act on. Filter on the node column.

- **Silence is not success.** A check that greps only the happy path stays quiet
  through a crash. Include the failure signatures you would act on.

## the shape, when you do need one

```sh
# poll an EXTERNAL job until terminal, then report which state it reached
for i in $(seq 1 20); do
  s=$(<status command> 2>/dev/null)
  case "$s" in *pending*|*running*|*queued*) sleep 90;; *) echo "$s"; break;; esac
done
```

Bounded, terminal-aware, and gone once it answers. If you find yourself writing
one against `hap`, stop — read the stream instead.
