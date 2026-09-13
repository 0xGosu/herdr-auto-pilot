# reviewing what an agent produced

Forge-independent. The `gh` commands are in [github.md](github.md).

An agent's summary is an argument, not evidence. Read it, then check each claim
against the artefact — the diff, the run, the file, the commit.

## the claims worth checking every time

- **"I fixed it."** Is the change in the diff? A reply on a review thread is not
  a fix.
- **"Tests pass."** Which command, with which tags or flags, and did the run you
  are being shown actually include the failing case? Ask for the command, not the
  verdict.
- **"It's flaky."** See below — this one is almost always asserted on too little.
- **"Done."** Mark a task done when its deliverable exists, not when the agent
  says so.

A good agent declares what it did **not** verify. Ask for that explicitly when it
is missing: *say plainly which of these you have not run.* The best report in one
session named its own unrun suites unprompted.

## flake or regression

Before accepting "flaky":

1. Reproduce under the conditions the failure used — the same flags, the same
   race or sanitiser setting.
2. Run the **same test on untouched main**, in a separate checkout, and compare
   failure rates.
3. State which it is, with the numbers.

Before accepting "regression", check whether the failing test even reaches the
changed code. Three plausible causal stories died that way in one session: the
commits touched no file in the failing package, changed no id allocation, and the
code they did change ran only in a command the tests never call.

**Rates matter more than verdicts.** At a 20-40% per-test failure rate, one
passing run proves nothing *in either direction* — neither "it passed when I
re-ran it" nor "a full green run" is evidence. Re-run the job and compare, or
measure the rate on main. A single green race run is closer to luck than proof.

## delegation has a scope

Merge, publish and delete are the operator's unless they said otherwise **for
these changes**. Permission given for one batch does not carry forward to work
they have not seen. When an older delegation might or might not cover what is in
front of you, ask — the cost of asking is a message; the cost of assuming is
unrecoverable.

Silence is never consent. An automated event that echoes your own action back at
you is not the operator answering.

## defects you were not asked to fix

Raise them where they outlive the session — an issue, with ids and timestamps —
rather than only in a report. Then say so and move on; do not start fixing
something nobody asked for.
