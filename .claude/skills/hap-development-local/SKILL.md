---
name: hap-development-local
description: "Develop and debug the Herd Auto Prompter (hap) plugin against a live herdr — link the working tree, rebuild the binary, and hot-swap the daemon so code changes take effect. Use when iterating on hap source inside a running herdr (HERDR_ENV=1) and you want the local build (not the installed release) to run."
---

# hap-development-local — local dev loop for the hap plugin

use this skill when you are **editing hap's Go source** and want the running
herdr to exercise your **working-tree build** instead of the installed release.
it is the developer counterpart to the `hap` skill (which *operates* an
already-installed plugin) and the `herdr` skill (which drives herdr itself).

before using this skill, confirm you are inside herdr (`HERDR_ENV=1`) and in the
repo (`/workspaces/herdr-auto-pilot`, or wherever the working tree lives). if
`HERDR_ENV` is not `1`, there is no live herdr to link against — say so and stop.

## the mental model (read this first)

herdr can run a plugin from a **local path** instead of a GitHub install. once
linked, herdr invokes the plugin's binary as **`./bin/hap` relative to the
plugin root** for every manifest command (daemon, tui, event hooks). so the
single source of truth for what herdr runs is the file at
`<plugin_root>/bin/hap`. rebuild that file, restart the daemon, done.

three moving parts, keep them straight:

- **`<plugin_root>/bin/hap`** — what herdr actually executes. Rebuild this.
- **the running daemon** — a long-lived process spawned from whatever binary
  last ran `hap daemon --ensure`. It does *not* pick up a new binary on its
  own; you must re-run `--ensure` to make it self-replace.
- **`/usr/local/bin/hap` (PATH symlink)** — a convenience symlink for *your*
  shell. **`herdr plugin link` does NOT repoint it** — see the gotcha below.

a local build reports version **`dev`** (`internal/buildinfo.Version`, stamped
only by release ldflags). that string is how you tell local-vs-release apart.

## one-time setup: link the working tree

```bash
herdr plugin link /workspaces/herdr-auto-pilot
```

- switches the plugin from `github:…@<sha>` to `local:/workspaces/herdr-auto-pilot`
  (`source.kind=local`, `plugin_root=/workspaces/herdr-auto-pilot`).
- **does NOT run the manifest `build`/`install.sh` step**, so it will not
  download a release binary over your `bin/hap`. Your build is safe.
- verify: `herdr plugin list` → `herd-auto-prompter … [local:/workspaces/herdr-auto-pilot]`.

### GOTCHA: fix the PATH symlink once

`herdr plugin link` leaves `/usr/local/bin/hap` pointing at the **old GitHub
install copy** (`~/.config/herdr/plugins/github/herd-auto-prompter-*/bin/hap`,
an *old* release version). So typing `hap …` in a shell still runs the stale
binary even though herdr's own invocations use your local build. Repoint it once:

```bash
ln -sf /workspaces/herdr-auto-pilot/bin/hap /usr/local/bin/hap
hap version   # → "hap (herd-auto-prompter) dev"  (confirms PATH = local build)
```

Alternatively, skip the symlink entirely and always call `./bin/hap` from the
repo root. But repointing it means `hap status` / `hap daemon --ensure` in any
shell hit your dev build, which is what you usually want.

To go back to the released plugin later: `herdr plugin unlink herd-auto-prompter`
then `herdr plugin install 0xGosu/herdr-auto-pilot` (and repoint or remove the
symlink).

## the iteration loop (every code change)

```bash
# 1. rebuild the local binary — CGO, ~5 min. Native deps must be present (below).
go build -tags "vectors cpu" -o bin/hap ./cmd/hap

# 2. hot-swap the daemon: the old daemon is flagged STALE (pid+version lock
#    mismatch) and self-replaces; the old pid exits cleanly.
hap daemon --ensure

# 3. confirm the swap took
hap status        # → "daemon: running dev (pid …)"  (no "STALE")
```

that's the whole loop. `bin/hap` → `daemon --ensure` → `status`. the daemon
does **not** hot-reload on its own — you must re-run `--ensure` after each
rebuild or herdr keeps running the previous process.

### the `--tags "vectors cpu"` is mandatory

the semantic matcher links native code (llama.cpp via CGO, FAISS behind bleve's
`vectors` tag). a build without **both** tags fails to link or compile:
- `vectors` — turns on bleve's FAISS-backed KNN vector search.
- `cpu` — disables llama-go's default GPU (Vulkan/Metal) linkage.

### native deps (only if the build fails to link)

CGO needs the native libraries once per environment. In the standard devcontainer
they are **already set up** (verified 2026-07): `libfaiss.so`/`libfaiss_c.so` in
`/usr/local/lib`, and `libllama.a` (a *static* archive, linked in — not a `.so`)
under `submodule/github.com/seed-hypermedia/llama-go/`. If a fresh environment
fails to link, run the one-time setup, then rebuild:

```bash
bash scripts/setup-native.sh   # submodules + llama-go libs + FAISS → /usr/local/lib
```

## reading the daemon state

`hap status` self-diagnoses a stale daemon — this is your signal that a rebuild
hasn't been swapped in yet:

```
daemon: running v0.3.42 (pid 925453) — STALE, binary is dev; run: hap daemon --ensure
```

means: the *running process* is an old release build, but the *binary on disk*
is now your local `dev` build. Run `hap daemon --ensure` to reconcile. After the
swap it reads plainly:

```
daemon: running dev (pid 1062410)
```

useful checks:

```bash
pgrep -af 'hap daemon'         # which binary path the live daemon runs
./bin/hap status               # bypass the PATH symlink, use the local build directly
```

the daemon's self-replace / health mechanics (heartbeat, crash-loop breaker,
pid+version flock) are issue #60's work — the version-mismatch detection that
powers this hot-swap lives there.

## live-testing a rebuild against real agents

once your `dev` daemon is running (above), drive a **real agent** to the state
you changed and watch how the daemon classifies it — this is the end-to-end
check the unit suite can't give you (it fakes herdr). the flow below is the one
used to verify the multi-tab MCQ (`choice`) parsing; adapt the prompt/assertion
for other situation types. it composes the `herdr` skill (spawn/drive panes) and
the `hap` skill (inspect what the daemon parsed).

**1 — isolate the test in its own workspace** so real work isn't disturbed:

```bash
herdr workspace create --cwd /tmp --label "hap-testing" --no-focus   # → note workspace_id (e.g. w4) + root_pane (w4:p1)
```

**2 — spawn a real agent** and wait for it to be ready. `pane run` sends the
launch command; use a cheap model to save tokens:

```bash
herdr pane run w4:p1 "claude --model haiku"
herdr pane read w4:p1 --source visible --lines 5   # confirm the ❯ prompt is up
```

**3 — prompt the agent to produce the situation you want to test.** GOTCHA:
`pane run` types the text but the agent TUI often does **not** submit on the
trailing Enter — the prompt sits in the input box. Send an explicit `Enter`:

```bash
herdr pane run w4:p1 "Do not write any code. Call your AskUserQuestion tool once with 3 multiple-choice questions in a single call: (1) language (Go, Python, Rust, TypeScript); (2) test framework (built-in, pytest, jest); (3) indentation (tabs, 2 spaces, 4 spaces). Ask all 3 together and wait."
herdr pane send-keys w4:p1 Enter                                    # actually submit it
herdr wait output w4:p1 --match "Language|programming language" --regex --timeout 45000
herdr pane read w4:p1 --source visible --lines 45                   # confirm the multi-tab form rendered
```

**4 — wait past the capture delay, then inspect what the daemon parsed.** The
first attention event for a fresh agent waits **10s** (`[[capture_delay]]`
`start_ms`) before the classification read, so give it ~12s:

```bash
sleep 12
hap agents          # your agent shows up (hap auto-names it, e.g. "deft-wren") with its status
hap escalations     # the parsed situation — type + suggestion
```

expected for the MCQ case — the situation classified as **`choice`** with a
multi-tab answer, e.g.:

```
#125  05:27:47  choice  [llm_low_confidence] llm confidence 96/100  agent=deft-wren  suggestion="LLM suggested: 1 1 1 2"
```

**5 — read the parse.** `hap escalations` / `hap audit --limit N` / the daemon
log (`grep w4:p1 "$(hap state-dir)/herd-auto-prompter.log"`) all show the
classification. cross-check the shape:

- the **situation type** is what you expect (`choice`, not `idle`/`approval`).
- a multi-tab MCQ answer has **one entry per tab in tab order, Submit included** —
  so a 3-question form yields a **4-token** answer (`1 1 1 2` = 3 answers +
  Submit). A well-formed N+1-entry answer that the daemon accepted is itself
  proof the form parsed into N question tabs + Submit, each with its options.
- two escalations for one form are normal — event bursts coalesce into a couple
  of capture bursts, each re-consulted.

### live-testing gotchas learned the hard way

- **`pane run` doesn't submit in an agent TUI** — always follow with
  `herdr pane send-keys <pane> Enter`. (Plain shells submit fine; agent input
  boxes hold the line.)
- **respect the 10s first-event capture delay** — check `hap escalations`
  *after* ~12s, or you'll see nothing and think the parse failed.
- **`get_context` is NOT replayable after the fact** — a `choice` consult's
  `llm_requests` row is consumed once the consult finishes, so
  `hap mcp` get_context returns "no pending decision request" post-hoc. To
  capture the exact parsed `context_json`, read it *while the consult is live*,
  or rely on `hap escalations` + the daemon log (which persist).
- **choice options aren't stored in `audit_log.input`** — the parsed MCQ
  structure lives in the pending-escalation memory, not the DB; don't expect to
  recover it from `audit_log` after resolving.
- **no `sqlite3` CLI in the devcontainer** — query the state DB read-only with
  Python: `sqlite3.connect("file:$DB?mode=ro&immutable=1", uri=True)` (the
  daemon holds it open in WAL mode).
- **closing a test agent without deleting its workspace** — an agent in a
  workspace's only tab: `herdr tab create --workspace w4` (add a scratch tab so
  the workspace survives) **then** `herdr tab close w4:t1` (the agent's tab).
  Closing the last tab directly can take the workspace with it.
- **`hap confirm` on a SHADOW escalation only learns — it does not send** — the
  audit row reads `resolved … corrected:<action> … operator confirmed`, and the
  menu stays up (shadow rules observe only). To also deliver, use
  `hap confirm <id> --send` (or `hap resolve <id> --action … --send`). Each
  confirm records a learning event and bumps the signature's confirmation count;
  once consistent confirmations reach `learning.graduation_n` **and** agreement
  clears the situation threshold, the signature graduates shadow→autonomous and
  the daemon starts auto-answering matches on its own. Verified live driving one
  agent to an approval: repeated `hap confirm`s promoted the rule and the
  recency-weighted agreement climbed `0.81 → 0.86 → 0.92 → 0.96` across
  successive auto-decisions (audit rows escalated `[shadow_mode]` → `auto`, each
  rationale "…chosen N times (agreement … > threshold …)"). This loop is pinned
  by `TestConfirmDrivenShadowToAutoPromotion` in `internal/daemon/daemon_test.go`.
- **once a signature goes autonomous, STOP hand-sending menu digits** — the
  daemon now answers that approval itself. If you also press the digit, both
  keystrokes land: one selects the menu, the extra digit drops into the
  now-empty input box as literal text (`❯ 1`) — cosmetic, but it looks like a
  bug and can prepend to the agent's next prompt. After a rule graduates, let
  the daemon own approvals; only intervene via `hap` (`confirm`/`resolve`/
  `dismiss`), never by racing keystrokes into the pane. (A test agent parked in
  `/tmp` with no task source also fires recurring `idle [no_task_source]`
  escalations between API retries — expected noise, ignore.)

## quick reference

| goal | command |
|---|---|
| link working tree | `herdr plugin link /workspaces/herdr-auto-pilot` |
| fix PATH symlink (once) | `ln -sf /workspaces/herdr-auto-pilot/bin/hap /usr/local/bin/hap` |
| rebuild | `go build -tags "vectors cpu" -o bin/hap ./cmd/hap` |
| hot-swap daemon | `hap daemon --ensure` |
| confirm swap | `hap status` → `running dev` |
| confirm PATH build | `hap version` → `dev` |
| revert to release | `herdr plugin unlink herd-auto-prompter` |

## gotchas recap

- **link does not repoint `/usr/local/bin/hap`** — fix it manually or use `./bin/hap`.
- **link does not overwrite `bin/hap`** — the build/install step doesn't run on link.
- **the daemon does not hot-reload** — always `hap daemon --ensure` after a rebuild.
- **omitting `vectors cpu` breaks the build** — both tags always.
- **`dev` is the tell** — a `dev` version = your local build; a `vX.Y.Z` version = a release.
