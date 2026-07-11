# Plan: classify Claude AskUserQuestion MCQ forms as `choice`

Status: investigation complete, fix not yet implemented.
Related: FR-002 (deterministic classification), FR-018 (unclassifiable fails safe to escalation).

## Problem

Claude Code's `AskUserQuestion` tool renders a multiple-choice question (MCQ)
form in the pane. The hap classifier does not recognize this shape: the
situation resolves to `unclassifiable` and the operator gets a bare escalation
with an empty suggestion instead of a `choice` escalation with extracted
options (and no chance for a learned rule or LLM consult to answer it).

Observed live on 2026-07-11 with two real Claude sessions:

| Escalation | Agent (pane) | Pane content | Result |
|---|---|---|---|
| #78 10:32:34 | vivid-yak (`w63:p1`) | single-question MCQ ("How do you want to submit to GitHub?") | `[unclassifiable] situation could not be classified` |
| #79 10:34:31 | crimson-moose (`w69:pC`) | multi-tab plan-mode question form (`← ☐ New name ☐ … ✔ Submit →`) | `[unclassifiable] situation could not be classified` |

Captured pane transcripts (raw, exactly as `herdr pane read --source visible`
returned them) live next to this plan:

- `samples/claude_mcq_single.txt` — single-question form
- `samples/claude_mcq_multi_tab.txt` — multi-question (tabbed) plan-mode
  form. NOTE: this is one frame only — the pane shows a single question at a
  time; the other tabs (`☐ Rename depth`, `☐ Config compat`, `☐ Conciseness`,
  `✔ Submit`) are captured by the Right-arrow sweep protocol described below.

## Root cause

`internal/classify/classify.go` — the built-in `choice` rule
(`DefaultRules`) has four patterns; none matches the AskUserQuestion layout.

1. The main numbered-menu pattern requires option 2 to start on the line
   **immediately after** option 1:

   ```
   (?m)^\s*[❯>]?\s*1[.)]\s+\S.*\n\s*[❯>]?\s*2[.)]\s+\S
   ```

   AskUserQuestion renders an indented description line under every option:

   ```
   ❯ 1. All 4 as REQUEST_CHANGES
        Post all 4 findings (Critical+Major+2 Suggestions) as inline comments, …
     2. Critical+Major only
   ```

   The description line breaks the adjacency, so the pattern never fires.
   (`.*` does not cross newlines, and the `\n` demands the very next line be
   option 2.)

2. The phrase fallbacks (`select an option`, `which option would you`, …)
   don't appear anywhere in the form. Its footer is:

   ```
   Enter to select · ↑/↓ to navigate · Esc to cancel          (single question)
   Enter to select · Tab/Arrow keys to navigate · Esc to cancel  (multi-tab form)
   ```

   No rule knows this footer.

3. No approval/error/idle rule matches either, and herdr reports the agent
   as `blocked`, so `Classify` falls through to `unclassifiable`
   (correct fail-safe behavior — the gap is purely the missing choice
   pattern).

Verified empirically against the live pane text: every current choice/approval
pattern returns no match; both candidate fixes below match both samples.

### What already works (no change needed)

`internal/domain/menu.go` (`ParseNumberedOptions` / `OptionLabels` /
`MenuKeystroke`) matches option lines individually, so once the situation is
classified as `choice`:

- option extraction works on both sample layouts (descriptions never start
  with a digit, so they are skipped);
- confirm/resolve delivery maps a chosen label to the menu digit as usual.

The extracted option set will include Claude's synthetic trailing entries
("4. Type something." / "5. Chat about this"). That is acceptable — they are
real selectable options and digit mapping stays correct.

## Multi-tab forms: capture ALL questions, answer as a series

`samples/claude_mcq_multi_tab.txt` shows only what the pane displays at any
one moment: **the currently focused question**. The tab header row

```
←  ☐ New name  ☐ Rename depth  ☐ Config compat  ☐ Conciseness  ✔ Submit  →
```

tells us there are more questions than the one on screen (4 questions + a
final "Submit" tab here), and the footer differs from the single-question
form: `Enter to select · Tab/Arrow keys to navigate · Esc to cancel`.

A single visible-pane read therefore under-captures the situation: the
signature, the escalation shown to the operator, and the LLM consult context
would all describe only question 1 of N. The plugin must sweep the form.

### Capture protocol (daemon side, before classify/escalate/consult)

1. **Detect the multi-tab variant**: tab header line containing `☐`/`✔`
   entries between `←` and `→`, plus the `Tab/Arrow keys to navigate`
   footer. Count the tabs from the header (checkbox entries + `Submit`).
2. **Sweep forward with the Right-arrow keystroke**: for each tab, read the
   pane (`--source visible`), capture the question text + numbered options,
   then send a **Right arrow** (`herdr pane send-keys <pane> Right`) to show
   the next question. Repeat until the last tab.
3. **Capture the "Submit" tab too** — when the sweep arrives at the final
   `Submit` tab, capture its content like any other question (it is answered
   as part of the series, see below).
4. **Reset to the first question**: send **10× Left arrow** (a fixed count
   larger than any real form's tab count) so the form is always back on the
   first question regardless of how many tabs exist. This leaves the form
   exactly as found, ready for answer delivery (or for the human operator if
   the situation escalates).
5. **Aggregate** the per-tab captures into one situation content block
   (question i/N + its options, in order). This aggregate — not the single
   visible frame — feeds signature generation, the escalation body, and the
   LLM `get_context` pane excerpt.

The sweep is pane interaction, so it must live off the daemon's main select
loop (same rule as `consultLLM`), be stall-guarded, and fail safe: if any
read/keystroke errors, fall back to the single-frame capture and escalate.

### Answer format: a digit series

The LLM (and learned rules) must answer a multi-tab form with a
**space-separated series of digits, one per tab including the final Submit
tab** — e.g. for the 5-tab sample above:

```
1 2 3 2 1
```

meaning: question 1 → option 1, question 2 → option 2, …, Submit tab →
option 1. The consult prompt / context must state this contract explicitly
(N answers expected for N tabs).

### Delivery

For each digit in the series, in order: deliver the digit to the pane (the
form selects that option and advances to the next tab on its own), continuing
through the Submit tab. Implementation notes:

- extend the delivery path (`domain.DeliverOutbound` callers) to recognize a
  multi-answer series for the multi-tab variant instead of mapping a single
  label to a single digit;
- validate the series length matches the captured tab count before sending;
  mismatch → escalate, never send a partial answer;
- safety gates (kill switch, never-auto allowlist, irreversible heuristic,
  rate guard) apply to the **aggregated** captured content plus every answer
  in the series, same as any other automated send.

## Fix plan

1. **Add a footer pattern to the built-in `choice` rule** (primary fix):

   ```
   (?i)enter to select
   ```

   This string appears in every AskUserQuestion variant (single and
   multi-tab) and in Claude's plan-mode question forms, and never in agent
   narration. Keep the `choice` rule *after* `approval` in `DefaultRules` —
   rule order encodes priority and permission menus must keep classifying as
   `approval` first (they also render numbered options).

2. **Loosen the adjacency pattern** (secondary, for panes where the footer
   is cropped out of the excerpt): allow a bounded run of indented
   description lines between option 1 and option 2:

   ```
   (?m)^\s*[❯>]?\s*1[.)]\s+\S.*\n(?:[ \t]+\S.*\n){0,4}\s*[❯>]?\s*2[.)]\s+\S
   ```

   The `{0,4}` bound keeps an ordinary narrated markdown list (numbered items
   with long wrapped/indented paragraphs) from false-positiving as a live
   menu.

3. **Golden fixtures**: copy the two captured samples into
   `internal/classify/testdata/transcripts/` as
   `choice_claude_mcq.txt` and `choice_claude_mcq_tabs.txt`, then
   `UPDATE_GOLDEN=1 go test ./internal/classify/` and review the diff —
   expected new golden lines classify both as `type=choice` with a non-zero
   option count (5 options each).

4. **Unit tests** (beyond the golden pin):
   - table cases in `classify_test.go` for both shapes;
   - assert extracted options include the real labels and the synthetic
     trailing entries;
   - a negative case: a narrated numbered list with a long indented
     continuation block (> 4 lines) must NOT classify as `choice`;
   - regression: existing approval fixtures (`approval_permission.txt`,
     `approval_yn.txt`) still classify as `approval`.

5. **Multi-tab sweep capture** (new daemon capability): implement the
   Right-arrow sweep / Submit capture / 10× Left-arrow reset protocol above
   behind an optional port (pane keystroke + visible read already exist:
   `ports.VisiblePaneReader`, send-keys). Off-main-loop, stall-guarded,
   fail-safe to single-frame capture.

6. **Digit-series answers**: teach consult prompt/context and the delivery
   path the "one digit per tab, Submit included" contract (e.g. `1 2 3 2 1`);
   validate series length against tab count; extend unit tests + fakeherdr
   coverage (sweep keystrokes observed, series delivered in order).

7. **Verify end-to-end**: rebuild (`go build -tags "vectors cpu" ./...`),
   `herdr plugin link .`, `hap daemon --ensure`, then drive a real Claude
   session to both AskUserQuestion variants and confirm `hap escalations`
   shows a `choice` escalation carrying ALL questions (multi-tab), and a
   confirmed answer series drives the form through Submit.

## Side note found during investigation

The running daemon was stale at investigation time (`hap status`: running
v0.2.0, binary v0.2.2 — flagged STALE). Unrelated to this bug (the classifier
gap exists in current source), but the fix rollout must include
`hap daemon --ensure` so the patched binary actually serves.
