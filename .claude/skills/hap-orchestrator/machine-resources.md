# the machine your agents share

Agents run on one box. Memory, CPU and any build that grabs all cores are
*shared*, and nothing in hap arbitrates them — that is your job.

## count processes by name, never by pattern

```sh
pgrep -xc cc1plus      # right: exact process name
pgrep -cf cc1plus      # WRONG: also matches your own waiting shell
```

`pgrep -f` matches against full command lines, **including the command line of
the shell running the `pgrep`**. A wait loop written that way counts itself, sees
a number that never reaches zero, and waits forever. This cost an orchestrator
and an agent about fifteen minutes each, independently, on the same day — the
box had been free the whole time.

## serialising a build across worktrees

A toolchain build can ignore every job-count control you set. Measured here: the
native submodule hardcodes `-j 8` inside its own Makefile, so `MAKEFLAGS`, an
outer `-j2` and `CMAKE_BUILD_PARALLEL_LEVEL` are all ignored and eight compilers
start regardless, at 500-600 MB each. Two agents doing that at once killed four
builds between them.

Three rules, each learned by breaking it:

1. **Check for the artifacts before assuming a build is needed.** A worktree
   keeps what it built. Both worktrees already held theirs; the entire hold was
   unnecessary.
2. **Serialise with an explicit go-ahead, not a process check.** "Wait until no
   compilers are running" passes immediately when the *other* agent has not
   started yet, and both begin together. Tell one agent to hold until you say so,
   and tell the other it has the slot.
3. **Give the slot to whoever is closest to delivering**, and let the held agent
   keep doing work that needs no build.

## memory pressure

```sh
free -h | awk 'NR==2{print "avail "$7}'
ps -eo rss,etime,comm --sort=-rss | head -10
```

Language servers are usually the largest resident consumers — one per worktree,
and they outlive the session that started them. Compilers are the largest
*transient* ones.

**Your own background watchers are what the kernel kills first.** Seven were
killed in one session while builds ran. They die harmlessly, but silently: a
watcher you believe is armed may be gone. So while a build is running, do not
re-arm watchers — take one-shot readings and lean on the event stream, which
lives in a different process.

Report consumption with numbers and let the operator decide what to reclaim. **Do
not kill processes you did not start**, even obvious orphans in another project —
offer, and wait.
