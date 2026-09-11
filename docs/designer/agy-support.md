# Design: agy (Antigravity CLI) as a first-class agent type — phase 1

**Date:** 2026-09-11 · **Status:** inventory + corpus + design; nothing implemented
**Captured against:** agy 1.2.1, herdr 0.8.2 (agy manifest `2026.06.24.1`), hap 0.9.16
**Corpus:** `internal/classify/testdata/transcripts/*_agy_*.txt` (31 fixtures, golden-pinned)

This is the plan phases 2–5 build on. Every claim below was observed live on scratch agy
agents (model `gemini-3.6-flash-low`, cwd a temp dir, hap disabled on each) unless it is
marked **unverified**.

---

## 1. The findings that shape everything

1. **herdr never reports an agy modal as `blocked`.** Every approval, question, picker,
   first-run and error screen captured arrived `idle` or `done` with
   `rule: none` / `fallback_reason: default_known_agent_idle_fallback`. The only agy rule
   herdr ever matched was `spinner_working`. So, as with Codex's Plan approval and Claude's
   remote-environment picker, **hap must detect every agy form structurally, while it is
   parked at idle/done.** Section 3 explains why herdr's rule misses.
2. **What hap does with agy today:** every agy screen classifies `idle` (the new golden rows;
   36 live audit rows from today agree — `type=idle` for approvals and MCQs alike). None of
   the claude/codex structural detectors run, and `DefaultRules()` has no agy error rule,
   so agy can never reach `SituationError` either.
   - **Hazard, unverified:** with a task source flagged `enable_auto_send_task_when_idle`,
     the idle poll would treat an agy approval as a parked agent and hand it a task.
     `agent prompt` is refused (`agent_blocked`) only when herdr says blocked, and herdr
     never does. Structural detection in phase 2 is therefore a **safety** fix as well as a
     feature.
3. **A digit COMMITS.** At a numbered agy menu, a single digit sent as a key (`pane
   send-text`) answers immediately. No Enter is needed. Verified on the shell approval
   (`1`, `4`), the file-access approval (`3`), the file-create approval (`1`), and both the
   one-question and two-question MCQs.
   - Where no numbered list is showing, the digit lands in the composer. The stray `4` and
     `0` in the corpus session prove it.
   - The unnumbered menus (trust folder, pickers, first-run) need arrows + Enter.
4. **Esc is not a safe probe.** At an approval it cancels the tool call, rendering
   `⎿  Interrupted · What should Antigravity CLI do instead?`. It does this even when the
   caret is inside the Tab-amend text field, where Esc was expected to return to the menu.
5. **agy has a 3-state permission-mode cycle on Shift+Tab: `default → accept-edits → plan →
   default`.** The indicator is a footer segment. `--dangerously-skip-permissions` adds no
   fourth mode and no indicator, and under it `rm` ran without a prompt. That means hap
   **cannot see** that mode from the pane.
6. **herdr 0.8.2's `send-keys shift+tab` now emits a real `ESC [ Z`.**
   - Verified with `/usr/bin/cat -v`, which shows `^[[Z`. The same test shows herdr's `tab`
     key name emits a bare TAB.
   - It also cycled agy's mode, and a bare TAB (`send-text $'\t'`) did not.
   - This contradicts the herdr-0.7.5 note in `CLAUDE.md`, and
     `TestRealShiftTabKeyNameIsStillBroken` should now FAIL on 0.8.2.
   - Keep `domain.ShiftTab` and `CLI.SendChord` anyway: `min_herdr_version` is 0.7.0, and
     CSI Z through `pane send-text` works on every version.
   - This is a separate follow-up, not phase-1 scope.
7. **agy renders inline, not on the alternate screen.** The launching shell line
   (`root ➜ /tmp/… $ agy --model …`) stays above the banner. A long session therefore scrolls
   into herdr's host scrollback, and `recent` reads return it.

---

## 2. Method and corpus

- **Agents:** scratch agents `agys1`–`agys3`, plus one bare `HOME=<tmp> agy` for first run.
  They ran in a scratch workspace with a temp cwd. Each was `hap disable`d, and all were
  cleaned up afterwards (workspace closed, temp dirs removed, the scratch
  `trustedWorkspaces` entry reverted).
- **Operator panes:** only read-only reads (quick-lemur `w1:p8E`). The first-run sign-in and
  terms screens are the operator's real `pane_excerpt` from the two hap escalations at
  17:32Z (agent htmx-4x-support, `w3:p1`, audit `#224220455398301696` and
  `#224220524226830336`, both `[no_task_source]`, both dismissed by the orchestrator).
  Reaching the terms screen needs a completed sign-in, so it was not reproduced.
- **What was saved:** `pane read --source visible|recent|recent-unwrapped`,
  `agent read --source detection`, `agent explain --verbose` and `agent get` per screen.
  Fixtures are the `visible` text with trailing blanks trimmed.
- **`recent` captures:** two screens are ALSO committed as the `recent` read, since that is
  the shape the daemon classifies: `approval_agy_shell_recent`, `choice_agy_mcq_two_recent`.
  They differ from `visible` in two ways:
  - a blank line separates every block, and the launch line and banner are included;
  - the MCQ capture still holds the EARLIER, answered question (`? Which fruit do you
    prefer?` / `> Banana`) above the live form.

  That second point is the two-render hazard §4 warns about. The other read sources and the
  `explain` output were session-local and are not retained beyond what this document quotes.
- **Redaction:** the account e-mail becomes `operator@example.com`, and the OAuth
  `client_id`, `code_challenge` and `state` become `<redacted>`.
- **Widths:** option labels WRAP to column 0 in a narrow pane. `approval_agy_shell_wrapped`
  (92 cols) and `approval_agy_shell_wide` (186 cols) are the same approval at two widths.

| Fixture | herdr status | Width | Target situation (phase 2) | hap today |
|---|---|---|---|---|
| `idle_agy_fresh` | idle | 189 | idle | idle |
| `idle_agy_after_turn` | done | 186 | idle | idle |
| `idle_agy_declined` | done | 92 | idle (the turn ended on `User declined the tool call`) | idle |
| `idle_agy_mode_accept_edits` / `_plan` | idle | 189 | idle; mode parse | idle |
| `idle_agy_composer_draft` | done | 92 | idle, composer NOT ready | idle |
| `idle_agy_shortcuts_overlay` | idle | 189 | operator UI (§4.11) | idle |
| `idle_agy_model_picker` / `_effort_picker` / `_slash_popup` | idle | 186 | operator UI (§4.11) | idle |
| `idle_agy_signin_method` | idle | 65 | first-run, operator-only (§4.10) | idle |
| `idle_agy_signin_url` (real) | idle | 188 | first-run, operator-only | idle |
| `idle_agy_terms` (real) | idle | 189 | first-run, operator-only | idle |
| `approval_agy_trust_folder` | idle | 189 | approval `trust` (§4.9) | idle |
| `approval_agy_shell` | done | 186 | approval `run` (§4.3) | idle |
| `approval_agy_shell_wide` / `_wrapped` | done | 186 / 92 | approval `run` | idle |
| `approval_agy_shell_amend` | done | 92 | approval with its text field open, never typed into | idle |
| `approval_agy_shell_plan_mode` | done | 92 | approval `run` (adds `ctrl+r Review`, `plan ·` footer) | idle |
| `approval_agy_file_access` | done | 92 | approval `access` (§4.4) | idle |
| `approval_agy_file_create` | done | 92 | approval `create` (§4.5) | idle |
| `approval_agy_artifact_review` | done | 92 | approval `review` (§4.6) | idle |
| `choice_agy_mcq` | done | 186 | choice, 1 question (§4.7) | idle |
| `choice_agy_mcq_two` / `_two_q2` | done | 186 | choice, question 1/2 and 2/2 | idle |
| `error_agy_interrupted` | done | 186 | error `interrupted` (§4.8) | idle |
| `error_agy_offline` | idle | 91 | error `eligibility check` | idle |
| `error_agy_model_warning` | idle | 91 | idle; a warning, not an error | idle |
| `working_agy_spinner` | working | 92 | working (herdr's `spinner_working` rule) | unclassifiable |

`TestGoldenTranscripts` gives every agy fixture its captured status and agent type `agy`
(any `_agy_` name). Phase 2 flips the "hap today" column, and the golden diff is its
before/after.

---

## 3. herdr's detection of agy

herdr's remote manifest (`agent-detection/remote/agy.toml` 2026.06.24.1) has three rules:

```toml
permission_prompt        blocked  contains "requesting permission for:"
                                  AND any( "do you want to proceed?" | ["tab amend","edit command"] )
spinner_working          working  line_regex ^\s*[⠀-⣿]+\s+\p{Alphabetic}+\w*ing\b
background_tasks_working working  line_regex (?i)·\s*[1-9][0-9]*\s+task   (bottom 5 lines)
```

`permission_prompt` evaluates ✗ on a screen that literally shows `Requesting permission
for:`, because agy 1.2.1's other anchors do not match its `any` list:

- The question is `Run this command?`, not `Do you want to proceed?`.
- The hint reads `tab Amend · ctrl+g edit/expand command`, and `edit command` is not a
  substring of that.

The file-access and file-create prompts carry neither anchor at all (their hint is
`↑/↓ Navigate` / `tab Amend · f full diff`), and MCQs, the trust prompt, pickers and
first-run screens have no rule. **Even a corrected herdr manifest would miss most of these**,
so hap cannot wait on upstream. Report the manifest drift upstream anyway: a `blocked` status
would also make `agent prompt` refuse an approval.

`spinner_working` does fire, on `⣟  Working...` / `⢿  Generating...`. `▸ Thought for 4s, 382
tokens` is a finished-thought marker, not a spinner. agy never exposed an `agent_session` in
`agent list` (claude does), so there is no session id from herdr (§6).

---

## 4. Situation catalogue

### Anatomy: the chrome every agy screen shares

```
root ➜ /tmp/x $ agy --model gemini-3.6-flash-low      ← launching shell line (inline render)
      ▄▀▀▄        Antigravity CLI 1.2.1               ┐
     ▀▀▀▀▀▀       operator@example.com (Google AI Pro)│ banner: 5 logo rows; text on rows 1–4:
    ▀▀▀▀▀▀▀▀      Gemini 3.6 Flash (Low)              │ version, account (plan), model (effort),
   ▄▀▀    ▀▀▄     /tmp/x                              │ cwd. First-run screens draw the logo
  ▄▀▀      ▀▀▄                                        ┘ with NO text beside it.
────────────────────────────── (full width)           ← turn separator
> <prompt as typed>                                   ← transcript: user turn
● Bash(ls -la /tmp/x) (ctrl+o to expand)              ← tool call; Read(…) Edit(…) Create(…)
  ⎿  User declined the tool call                      ← tool result / notice
  <assistant reply, 2-space indent>
────────────────────────────── (full width)           ┐
>                                                     │ composer sandwich
────────────────────────────── (full width)           ┘
? for shortcuts                 [mode · ]Gemini 3.6 Flash · low   ← footer status bar
```

- **The footer's left token is the state signal:**

  | Left token | Meaning |
  |---|---|
  | `? for shortcuts` | Composer empty and ready |
  | nothing | A draft is in the composer (`idle_agy_composer_draft`) |
  | `esc to cancel` | Working, a modal is up, or the slash popup is open |

  The right side is `[accept-edits · |plan · ]<model>[ · <effort>]`. The effort segment
  vanishes after `/model` is dismissed, and the whole right side is absent when agy could
  not load a model (`error_agy_offline`).
- **A mode placeholder occupies the EMPTY composer line in the two non-default modes:**
  `> Accept-edits mode: file edits auto-approved (shift+tab to cycle)` and
  `> Plan mode: research & plan only (shift+tab to cycle)`. It is placeholder text, not a
  draft, like Codex's composer placeholder.
- **Other right-aligned chrome:** `1 artifact · /artifact to review` above the composer,
  and a transient feedback survey (§4.12).

### Salient rules (phase 2: `StripAgyChrome`, the analogue of `StripClaudeChrome`)

These are pane-tail salients, gated on agent type `agy`. Every filter must be ANCHORED and
positively identified, following the `StripClaudeChrome` rules in `CLAUDE.md`: an
unrecognized line is kept.

- **Banner:** armed ONLY by `Antigravity CLI <semver>` on a row whose left part is logo glyphs.
  Strip that row and the next four rows when their left part is only `▄▀` glyphs and blanks.
  Their text (account, `Gemini … (…)`, cwd) goes with them. The model line is chrome: it
  differs per session and would otherwise split one situation into a signature per model.
  - First-run screens draw the logo with no `Antigravity CLI` text beside it. Strip
    logo-only rows there too, but only rows made entirely of `▄▀` and spaces with ≥4 glyphs,
    so a progress bar (`[████…]`, which is `█`) survives.
- **Launching shell line:** a line whose command is `agy` (`… $ agy …`) directly above the
  banner.
- **Composer sandwich + footer:** the LAST `─` rule / `>` line / `─` rule triple and the
  line under it.
  - Anchor on "last non-empty lines", never on a bare `>`: agy also uses `>` as the caret in
    every menu (`> 1. Yes, run command`, `> Yes, I trust this folder`).
  - Recognize the footer by its left token (`? for shortcuts` or `esc to cancel`, or empty)
    AND a right-aligned `Gemini|Claude|GPT… [· low|medium|high]` segment after a padding
    run of ≥10 spaces. This is the "`esc to cancel … Gemini … high` status bar" the task names.
- **Tool-call suffix** ` (ctrl+o to expand)` and the right-aligned `N artifact(s) ·
  /artifact to review`.
- **Mode placeholders** (above) → an empty composer.
- **Keep:** transcript turns (`> …` user lines ABOVE the composer), `●` tool lines, `⎿`
  results, assistant text, and every form body.

### Structured salients (short by construction; exempt from `min_salient_chars`)

Every form below is parsed from the LAST render in the capture. A `recent` capture can
hold an earlier render: the two-question MCQ prints `? What is your favourite colour?`
headers above the live form. Salients go through `MaskVolatile` like every other salient.

### 4.1 Idle — ready composer

- **Recognize:** herdr `idle`/`done` + the composer sandwich + a footer whose left token is
  `? for shortcuts`, with no form (below) standing.
- **Salient:** the chrome-stripped tail.
- **Answer:** a hand-out via `agent prompt`. A multi-line prompt landed as ONE message
  (`idle_agy_…` session: two lines, one `PONG`).
- **`AgyComposerReady` (phase 3)** requires, from the bottom up:
  1. the footer with a `? for shortcuts` left token;
  2. directly above it, a `─` rule;
  3. a `>` line that is empty or exactly a mode placeholder;
  4. a `─` rule;
  5. no survey line and no form anchor anywhere after the last `●`/turn separator.

  A draft removes `? for shortcuts`, and a modal or a working turn replaces it with
  `esc to cancel`. The survey is the one overlay seen coexisting with `? for shortcuts`,
  hence clause 5.

### 4.2 Working

- **Recognize:** herdr `working` (the spinner rule), or a braille spinner line
  `⣟  Working...` with footer `esc to cancel`.
- The composer sandwich is drawn while working, so agy likely QUEUES input as claude does
  (**unverified**). That is why the proof above needs `? for shortcuts` and not the
  sandwich alone.

### 4.3 Shell-command approval (`approval_agy_shell*`)

```
Command
────────────────
Requesting permission for:
   rm -rf /tmp/x/hello.txt
Run this command?
> 1. Yes, run command
  2. Yes, and always allow in this conversation for commands that start with 'rm -rf /tmp/x/hello.txt'
  3. Yes, and always allow for commands that start with 'rm -rf /tmp/x/hello.txt' (Persist to settings.json)
  4. No, cancel
  ↑/↓ Navigate · tab Amend · ctrl+g edit/expand command[ · ctrl+r Review]
esc to cancel                                        Gemini 3.6 Flash · low
```

- **Classify:** approval, parked at idle/done.
  - **Anchors:** `Requesting permission for:`, then `Run this command?`, then numbered options,
    then a hint line starting `↑/↓ Navigate`.
  - **Command:** the indented block between the first two anchors, which may wrap.
- **Verb:** `run`.
- **Options:**
  - A label runs from `N. ` to the next `N. ` or the hint line. Continuation rows are
    **joined**, and in a narrow pane they start at **column 0**
    (`approval_agy_shell_wrapped`). This is unlike Claude, whose continuations are indented,
    so the column-0 continuation needs its own rule in the option parser.
  - A long command also lengthens options 2 and 3, since they embed its prefix.
- **Salient:** `permission:run | command:<masked command> | options:yes, run command;yes,
  and always allow in this conversation for commands that start with '<prefix>';yes, and
  always allow for commands that start with '<prefix>' (persist to settings.json);no,
  cancel`.
  - Replace the quoted prefix with `<prefix>`, so one rule covers the menu's shape and the
    command is carried by `command:`.
- **Answer:** the option's digit as a key (`pane send-text`), with **no Enter**. It commits on
  the keypress: `1` ran the command, and `4` produced `⎿  User declined the tool call`.
- **Safety:**
  - Options 2 and 3 widen agy's own allowlist, and option 3 persists it to
    `settings.json`. Both are never-auto seed candidates. So is the `Yes, and always allow
    non-workspace access` option (§4.4, §4.5).
  - The command runs through `internal/domain`'s irreversibility heuristic like any other.
    agy prints commands verbatim (`rm -rf …`), so the existing corpus should cover them;
    phase 2 checks.
  - **Tab** opens an inline amend field (`approval_agy_shell_amend`: option 1 becomes
    `Yes, and tell Antigravity CLI what to do next` with `> █` under it). Its footer is
    `enter Submit`, and Esc there CANCELS the call. hap must never type into it: treat that
    render as "operator is typing" and do nothing.

### 4.4 Non-workspace file access (`approval_agy_file_access`)

```
File access
────────────────
Read: /etc/hostname
Reason: outside workspace
Allow access to this file?
> 1. Yes, allow access
  2. Yes, and always allow non-workspace access
  3. No, deny access
  ↑/↓ Navigate
```

- **Classify:** approval.
  - **Anchors:** `Reason: <text>`, then `Allow access to this file?`, then numbered options.
  - **Operation and path:** the `<Op>: <path>` line above (`Read:` seen; other verbs are
    **unverified**).
- **Verb:** `access`.
- **Salient:** `permission:access | op:read | reason:outside workspace | options:…` (path
  masked).
- **Answer:** the digit, which commits (`3` → `User declined the tool call`).

### 4.5 File create / edit approval (`approval_agy_file_create`)

This is agy's "file-edit approval".

- **Observed:**
  - Default mode did **not** prompt for a create inside the workspace, nor for an edit of
    an existing file under `/tmp`.
  - It prompted when the path left the workspace and was not under `/tmp`: here the plan
    artifact under `~/.gemini/antigravity-cli/brain/…`.
  - Whether a stricter `Tool Permission` setting (`strict`) prompts inside the workspace is
    **unverified**: it is a global setting in the operator's `settings.json`, deliberately
    not changed.
- **Screen:** the file path, a numbered `+` diff (`… N more lines (f for full diff)`),
  `Reason: outside workspace`, then `Allow creation of this file?` with options
  `1. Yes, allow creation` / `2. Yes, and always allow non-workspace access` /
  `3. No, deny creation`, and hint `↑/↓ Navigate · tab Amend · f full diff`.
- **Classify:** approval, verb `create`. Edit is presumably `Allow edit of this file?`
  (**unverified**), so match `Allow (\w+) of this file\?` and take the verb from it.
- **Salient:** `permission:create | reason:… | options:…`. The diff body stays in the LLM
  excerpt and out of the salient, or every file mints its own rule.
- **Answer:** the digit (`1` allowed it, and the tool ran).

### 4.6 Plan-artifact review (`approval_agy_artifact_review`)

- **When:** in `plan` mode agy writes an implementation-plan artifact, then says `Please
  review the implementation plan artifact and click Proceed`. The footer gains
  `1 artifact · /artifact to review`.
- **The panel:** `/artifact` opens
  `Action required (N left)` / `› □ new <name>.md   open  approve reject` with keys
  `y/n Approve/reject  shift+a Approve all  p Preview  ctrl+g open in editor  esc Done`.
- **Classify:** approval, verb `review`, parked, from the `Action required (N left)` anchor.
- **Salient:** `review:artifact | items:<N> | kinds:new`.
- **Answer:** `y` approves the focused item and `n` rejects it (verified: `y` → `⎿  Review
  submitted`, `> [Approved] create_plan_txt.md`, execution resumed). **Never `shift+a`**,
  which approves every artifact unseen.
- **This is agy's Plan approval:** the counterpart of `CodexPlanApprovalForm`. It is only
  reachable while the panel is OPEN. Before that, the pending review is just the
  `N artifact · /artifact to review` marker on an idle screen. Phase 3 decides whether hap
  opens the panel itself (`/artifact` + Enter into a proven-empty composer) or escalates the
  marker. Recommendation: escalate, as an approval whose salient is the marker plus the
  artifact name.

### 4.7 Multiple-choice question (`choice_agy_mcq*`)

```
? Which fruit do you prefer?
Question
────────────────
Question 1/1: Which fruit do you prefer?
> 1. Apple
  2. Banana
  3. Cherry
  4. Write-in...
  ↑/↓ Navigate · enter Select · esc Skip          (question i>1 adds "· ← Back")
```

- **Classify:** choice, parked.
  - **Anchors:** `Question <i>/<N>: <text>`, then numbered options, then a hint line starting
    `↑/↓ Navigate` that contains `enter Select`.
  - **New `MCQKind`:** `agy_questions`, alongside `claude_tabs` and `codex_questions`.
  - **The form shows ONE question at a time.** Questions not yet shown are listed only as
    `? <text>` headers above it, without their options, so options for question 2 are
    unknowable until question 1 is answered.
- **Salient:** `choice | question:<i>/<N> <masked text> | options:<labels minus Write-in...>`.
- **Answer:** one digit per question, no Enter. Verified:
  - A digit commits AND advances: `3` moved 1/2 → 2/2.
  - The last digit submits. There is no Submit/review tab. `1` → `Thank you! You selected
    Blue … and Cat`.
  - So a series `3 1` is two keypresses, each re-verified against a fresh read of the next
    question, the way `codexSeries` does.
  - `Write-in...` expects free text (**unverified**); refuse to auto-answer it.
  - `← Back` exists, so a stale second question can be walked back. `esc Skip` skips a
    question and is never sent.

### 4.8 Errors

- **`error_agy_offline`:** `⚠ Eligibility Check` / `⎿  Eligibility check failed: Post
  "https://…/v1internal:loadCodeAssist": proxyconnect tcp: …`.
  - It prints after launch and again after every prompt, so the agent cannot progress.
  - **Classify:** error.
  - **`ErrorSummary`:** `eligibility check failed`.
  - **Salient:** `error:eligibility-check | <first message line, masked>`.
  - It is a network/auth fault, so an automatic answer never fixes it: escalate.
- **`error_agy_interrupted`:** `⎿  Interrupted · What should Antigravity CLI do instead?`
  as the result of the last turn.
  - **Classify:** error (`interrupted`), the analogue of `ClaudeErrorForm` /
    `CodexErrorForm`'s interrupted forms.
  - It must be the LAST transcript item above the composer, or an old interrupt in
    scrollback reads as live.
- **`error_agy_model_warning`:** `⚠ Warning` / `⎿  model bogus-model-xyz is not recognized …
  Using "Gemini 3.8 Flash (High)" instead.`
  - The agent is usable, so this is **not** an error: classify idle and strip nothing.
  - Note that it silently fell back to the most EXPENSIVE model.
- **Quota / rate-limit errors:** not reproducible on this account (`/usage` shows
  weekly and five-hour limits at 99%+). Their text is **unverified**, and phase 2 must not
  guess a regex. `/usage` shows the vocabulary to expect (`Weekly Limit`, `Five Hour
  Limit`, `Refreshes in …`).
- **`idle_agy_declined`:** `⎿  User declined the tool call` ends the turn cleanly. It is
  idle, not an error.

### 4.9 Trust folder (`approval_agy_trust_folder`)

- **Screen:** `Accessing workspace:` / `<cwd>` / `Do you trust the contents of this
  project?`, then UNNUMBERED options `> Yes, I trust this folder` / `  No, exit`, and hint
  `↑/↓ Navigate · enter Confirm`.
- **When it appears:** at launch in any untrusted cwd. That includes every worktree an
  orchestrator starts an agy in.
- **Classify:** approval, verb `trust`, parked.
- **Salient:** `permission:trust | options:yes, i trust this folder;no, exit`.
- **Answer:** no digits. Move the caret with `up`/`down` (verified), re-read that it rests on
  the chosen row, then `enter`. The caret starts on `Yes`. `No, exit` quits agy.
- **Safety:** trusting grants read/edit/execute in the folder. Seed a never-auto pattern and
  keep it operator-only until someone decides otherwise.

### 4.10 First-run: sign-in and terms — operator-only, always

- **`idle_agy_signin_method`:** `Welcome to the Antigravity CLI. You are currently not
  signed in.` / `Select login method:` / `1. Google OAuth` / `2. Use a Google Cloud
  project`. The Cloud path asks `Select Google Cloud sign-in method:` next.
- **`idle_agy_signin_url` (real):** `Your browser should open automatically. If not:` + the
  OAuth URL + `→ Click here to authenticate` + `paste the authorization code below:` /
  `authorization code...`.
- **`idle_agy_terms` (real):**
  - `Terms of Service & Data Use`, then a `[x] Yes, I agree to help improve …` data-sharing
    toggle and `[Previous]  [Done]`.
  - Hint: `↑/↓ Navigate · enter Toggle`.
  - The keys that reach `[Done]` are **unverified**: it needs a completed sign-in.
- **Classify:** a new `idle`-parked structured situation, `setup`.
  - Escalate with a fixed, plain summary: "agy needs sign-in" / "agy needs the operator to
    accept its terms".
  - **Never** hand these to the generate-task LLM. Today they reach it and come back with
    suggestions like "Complete Google authentication…" and "select Done".
  - **Never** deliver keystrokes: sign-in needs a browser, and `enter Toggle` on the terms
    screen FLIPS the data-sharing consent.
- **Salients:**
  - `setup:signin | method-select`
  - `setup:signin | oauth-url` (the URL is dropped, as it is volatile)
  - `setup:terms`

### 4.11 Operator UI: pickers, panels, overlays — do nothing

- **What these are:** `/model` (`Switch Model` list + an `Effort ◂ ◉──○──○ ▸` slider),
  `/effort` (`Set Effort`), `/permissions` (`Permission Config Editor`), `/config`
  (`Settings`), `/usage` (`Models & Quota`), the `?` help (`general / commands / shortcuts`
  tabs), and the slash popup (`> /model  Set a model…` with `↑/↓ Navigate · enter Select ·
  tab Complete`).
- **They are there only because a human opened them.**
- **Classify:** idle, composer NOT ready. Their footers never carry `? for shortcuts`, and
  most end in a `Keyboard: …` line. hap sends nothing, escalates nothing, and hands out
  nothing.
  - **Anchors:** `Switch Model`, `Set Effort`, `Permission Config Editor`, `Settings` +
    `Search:`, `Models & Quota`, `Antigravity CLI   general    commands    shortcuts`.
  - The generic ones are the last line `Keyboard: …` and a hint containing `enter Select`
    with no `Question i/N:` above it.
- Esc closes each one. Never needed by hap.

### 4.12 Feedback survey — a transient digit trap

- **Screen:** `How's the CLI experience so far? Help us improve:` / `[1] Good  [2] Fine
  [3] Bad  [0] Skip`. It appeared after a turn, above a `? for shortcuts` footer, and was
  gone seconds later.
- **The trap:** a digit sent while it stands answers the SURVEY. A digit sent after it
  closes lands in the composer.
- **Handling:**
  - Add it as a hard refusal clause in `AgyComposerReady` and before every digit delivery.
  - Never answer it. If a menu answer is due, re-read until the survey is gone, bounded.
  - It is not in the committed corpus: the snapshot after it had already cleared. The text
    above was read live. Capture it as a fixture when it reappears.

---

## 5. Keystrokes

| Screen | Keys hap may send | Transport | Verified |
|---|---|---|---|
| Numbered approval (§4.3–4.5) | the option digit, **no Enter** | `pane send-text` (a key, never paste) | ✔ `1`, `3`, `4` |
| MCQ (§4.7) | one digit per question, re-read between | `pane send-text` | ✔ 1-q and 2-q |
| Artifact review (§4.6) | `y` / `n` | `pane send-text` | ✔ `y` |
| Trust folder (§4.9) | `up`/`down` to target, re-read, `enter` | `pane send-keys` | ✔ caret moves; `enter` on Yes |
| Hand-out / resolve text (§4.1) | the text, then Enter | `agent prompt` (paste-aware); single-line may use `send-text` + `enter` like the other kinds | ✔ multi-line = one message |
| Mode cycle (§6) | CSI Z | `pane send-text "\x1b[Z"` | ✔ (herdr 0.8.2 `send-keys shift+tab` also works) |

**Never sent:**

- `esc` (it cancels an approval, including from the amend field)
- `tab` at an approval (it opens amend)
- `shift+a` (approve all)
- anything into the survey, a picker, the amend field, sign-in or terms
- `2`/`3` "always allow" options, unless an operator's rule names them explicitly (seeded
  never-auto)

`domain.UnmatchedMenuReply` applies unchanged: a label mapping to no option must not be
delivered, because agy has the same caret-commits-on-Enter shape Claude has.

---

## 6. Permission modes and sessions (phase 4)

- **Modes:** `default`, `accept-edits`, `plan`, cycled with Shift+Tab (CSI Z) in that
  order, a closed rotation of 3.
  - Settable at launch with `--mode accept-edits|plan` and persisted by `/config → Agent
    Mode`.
  - **Indicator:** the footer segment before the model (`accept-edits · …`, `plan · …`;
    default has none), plus the composer placeholder while it is empty.
  - Match on the LABEL, and read absence as `default` only once the footer itself is
    recognized: the same rule `CodexAgentMode` follows.
  - A press cap of 3 is enough, and the rotation detection `SetAgentMode` already does
    applies unchanged.
- **Unseen modes:** `--dangerously-skip-permissions` is invisible in the pane, so `hap mode`
  must not claim to know it.
- **Session id:** herdr exposes no `agent_session` for agy. agy itself has
  `--conversation <ID>` / `--continue` and stores conversations under
  `~/.gemini/antigravity-cli/conversations/`. No captured screen shows an id (`/rename`
  exists but was not exercised). Session-id extraction is therefore **not available** from
  the pane. Phase 4 should skip it unless print mode (`-p --output-format json`) is used for
  the LLM path.

---

## 7. Where agy plugs in (inventory of per-type touchpoints)

hap has **no agent-type enum**: herdr's `agent` string is stored verbatim
(`internal/herdr/cli.go` `AgentType: a.Agent`), and `domain.IsPlaceholderAgent` drops only
`""`/`undefined`/`unknown`. So agy is already monitored, subscribed and audited. It only gets
the agent-agnostic path. `internal/llm/normalize.go` already has `agy` argv repair, and
`strictmcp.go` deliberately skips agy.

| Phase | Package | Touchpoint | agy work |
|---|---|---|---|
| 2 | `domain` | `claudechrome.go` `StripClaudeChrome`, `signature.go` `SalientContent` | add `StripAgyChrome` (§4 rules), gate on `agy` |
| 2 | `domain` | `clauderemoteenv.go` / `codex.go` form extractors | new `agy.go`: approval, MCQ, trust, review, setup, and operator-UI detectors |
| 2 | `domain` | `codexerror.go`, `claudeerror.go` | new `agyerror.go`: interrupted + eligibility |
| 2 | `domain` | `mcqform.go` `ParseMCQForm` / `ExtractAgentMCQForm` / `AggregateAgentMCQFrames`, `codex.go` `MCQKind` | `MCQAgyQuestions` |
| 2 | `domain` | `menu.go` `ParseNumberedOptions`, `MenuSituation` | column-0 continuation joining; the agy approval counts as a menu |
| 2 | `classify` | `DefaultRules` scoped error rules; the codex/claude pre-strip and structural flags (`classify.go` ~130–307); approvals allowed while parked | agy error rule + parked-form flags |
| 2 | `domain/testdata` | `irreversible_corpus.txt` | add any agy-specific destructive phrasings |
| 3 | `herdr` | `cli.go` `sendBehavior` (agy gets NO retry Enters today); `paneShowsStandingForm` | decide retry-Enter; add the agy standing forms |
| 3 | `deliver` | `deliver.go` form dispatch; `claudetabs.go` `tabSeries`; `codex.go` `codexSeries` | `agySeries` (digit, re-read, next) |
| 3 | `domain` | `agentmode.go` `ComposerReadyForMode` | `AgyComposerReady` (§4.1) |
| 3 | `daemon` | `sweep.go` MCQ dispatch; `daemon.go` LLM excerpt strip (`StripCodexComposer` site) + MCQ kind prompt text; `autoaccept.go` | agy branches |
| 3 | `mcpserver` | `mcpserver.go` compares the literal `"codex_questions"` | mirror the new kind |
| 4 | `domain` | `agentmode.go` `AgentModesFor`, `ModePressCap`, `AgentModeFromPane` | agy modes (§6) |
| 4 | `cli` / `tui` / docs | `cli.go` `modeReadError` text "(claude and codex do)", help mode list, TUI "Claude / Codex / others" label, README, `CLAUDE.md`, hap skill | text |
| 4 | `fakeherdr` | carries any agent label already | fixtures for agy tests |
| 4 | `frontend` | `llmpreset.go` | optional agy preset |
| 5 | `test/integration` | `requireClaude`/`requireCodex`, `startAgentInPane` | `requireAgy` + `HAP_ITEST_AGY=1` |

The following do **not** need an agy branch:

- `domain.AgentModesFor` returning nil already makes `hap mode` refuse cleanly.
- `ComputeSignature` partitions by type automatically.
- The capture delay is config-only.
- The orchestrator stays claude-only.

---

## 8. Open questions / unverified

- The keys that reach the terms screen's `[Done]`: needs a completed sign-in (§4.10).
- The wording of the file-EDIT approval (`Allow edit of this file?` presumed), and whether
  `Tool Permission = strict` prompts for in-workspace edits (§4.5).
- Quota / rate-limit error text (§4.8).
- Whether agy queues typed input while working (§4.2).
- `Write-in...` free-text answers (§4.7).
- The survey's exact render and lifetime (§4.12).
- The **hand-out-into-modal** hazard (§1.2) is inferred from the code path, not exercised.
  Phase 2's detection closes it for the idle poll: the capture re-classifies as approval or
  choice, and every hand-out branch requires the IDLE situation. A generated-task `--send` is
  not covered, because `refuseIfAgentBusy` reads herdr's status only (which reads idle under an
  agy modal). Phase 3's composer-ready proof closes that path.
- `TestRealShiftTabKeyNameIsStillBroken` should now fail on herdr 0.8.2 (§1.6). Run
  `HAP_ITEST_CLAUDE=1` to confirm, then update the `CLAUDE.md` gotcha. Keep the CSI Z path
  for older herdr.

---

## 9. Phase 2 as built (domain + classify)

What shipped, and where it deliberately departs from §4:

- **Type and kind.** `domain.AgentTypeAgy` and `domain.CanonicalAgentType`. The latter folds
  herdr's manifest aliases (`antigravity`, `antigravity-cli`) onto `agy` where herdr's labels
  enter hap: `herdr.CLI.ListAgents` and the event subscriber. Every other label is untouched.
  `domain.IsAgy` is the gate everywhere else.
- **Parsers** (`internal/domain/agy.go`), each anchored to the bottom of the capture (only the
  status bar and blank lines may follow the form's key-hint line):
  - `ParseAgyApproval` — the shell and file prompts. Column-0 wraps are rejoined, so wide
    and narrow renders share labels.
  - `ParseAgyTrust`, `ParseAgyReview`, `ParseAgyMCQ` (Write-in excluded).
  - `AgyErrorForm` — `interrupted`, `eligibility-check-failed`. Both must be the last
    transcript item above a live composer.
  - `AgyHeldForm` — setup and operator UI.
- **Classification.**
  - The approval and choice forms classify as parked at idle/done, like Codex's Plan approval,
    and also when blocked. Working is excluded.
  - Errors classify ungated, like claude/codex.
  - `DefaultRules` gained the scoped agy error rule.
  - **Departure from §4.10–4.12:** setup (sign-in, terms) and operator UI (pickers, panels,
    the slash popup, the Tab-amend field, the survey) classify **unclassifiable**, not a new
    `setup` situation. That outcome escalates with no LLM consult, no suggestion and no
    keystroke. A new situation type would ripple through Decide, the store and every front
    end for no safety gain. They are held ahead of every rule, the operator's included.
- **Salients.**
  - Approvals: `permission:<verb> | options:…`.
    - Verbs: `run command: <cmd>`, `<verb> file (<target>; <reason>)`, `trust this folder`,
      `review plan artifact`.
    - The verb is stored UNMASKED: `IrreversibleScanContent` reads it raw, and masking hides
      `of=/dev/sda`.
  - Choices: `options:…`.
  - Errors: `error:<kind>`.
  - Idle: pane tail through `domain.StripAgyChrome`. It removes the launch line, the banner
    (also a scrolled-in fragment of it), rules, the spinner, the artifact marker, the
    `(ctrl+o to expand)` suffix, and the composer + status bar. A capture that is nothing but
    chrome gets one fixed salient instead of an over-masked empty one, which would escalate
    before the task source is ever consulted.
- **Replies are withheld** (`domain.AgyReplyWithheld`, `deliver.ErrReplyWithheld`,
  `domain.ReasonReplyWithheld`).
  - **Why:** every send path types the digit and then Enter, and agy commits on the digit
    alone.
  - **Where it is refused:** all four send paths (`daemon.act` ahead of the action-review
    dispatch, the action-review outcome, the LLM promotion, and `deliver.Deliver`), for agy
    approvals and choices.
  - **Auto-accept:** a `reply_withheld` escalation is excluded outright
    (`autoAcceptExcludedReasons`). An agy form escalated for any other reason is claimed, and
    the refusal is read as `errOutboundRefused`, so it never burns the attempt budget or
    dismisses the row (`TestAutoAcceptLeavesAWithheldAgyReplyPending`).
  - **What is not withheld:** idle hand-outs and error replies are free text into the
    composer and still go.
  - Phase 3 lifts the gate form by form as the agy deliverer lands.
- **No `MCQKind` / `AnswerCount` for agy yet.** Either routes a form into the multi-question
  sweep and the Claude/Codex deliverers, which press Right/Left into the pane before they
  refuse. `domain.MCQAgyQuestions` is declared for phase 3 but never set, and
  `domain.ParseMCQForm` still answers false for agy. The agy question form is matched at the
  choice position directly.
- **Corpus.** `irreversible_corpus.txt` gained agy-rendered lines (the command between agy's
  anchors, and an "always allow" option carrying a force-push prefix). The existing seed
  patterns already match them, because agy prints commands verbatim.
