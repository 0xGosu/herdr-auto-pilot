package domain

import (
	"reflect"
	"strings"
	"testing"
)

const mcqTabFrame = `⏺ Exploration done. Full map of escalation strings + allowlist usage.
───────────────────────────────
←  ☐ New name  ☐ Rename depth  ☐ Config compat  ☐ Conciseness  ✔ Submit  →

New name for the "allowlist" concept (config key, reason token, labels)?

❯ 1. never_auto (Recommended)
     config key ` + "`never_auto_patterns`" + `, reason token, TUI/CLI label "never-auto".
  2. deny
     config key ` + "`deny_patterns`" + `. Shorter but ambiguous (deny what?).
  3. escalate_always
     config key ` + "`escalate_always_patterns`" + `. Very explicit, longer.
  4. Type something.
───────────────────────────────
  5. Chat about this

Enter to select · Tab/Arrow keys to navigate · Esc to cancel
`

// mcqTabFrameV2 uses the Claude Code v2.1.207 footer wording — the tab-switch
// hint moved from "Tab/Arrow keys to navigate" to "Tab to switch questions"
// (issue #50). The header is unchanged, so only the footer regex must adapt.
const mcqTabFrameV2 = `⏺ Daemon dead — no process. Two decisions are yours:
───────────────────────────────
←  ☐ Test scope  ☐ Daemon  ✔ Submit  →

How deep should the feature test go?

❯ 1. Read-only + config
  2. Full end-to-end
  3. Read-only only
───────────────────────────────
  Chat about this

Enter to select · ↑/↓ to navigate · n to add notes · Tab to switch questions · Esc to cancel
`

func TestMultiTabFormDetectsTabs(t *testing.T) {
	tabs, ok := MultiTabForm(mcqTabFrame)
	if !ok || tabs != 5 {
		t.Fatalf("MultiTabForm = (%d,%v), want (5,true)", tabs, ok)
	}
}

func TestMultiTabFormDetectsV2Footer(t *testing.T) {
	// Regression for #50: the v2.1.207 "Tab to switch questions" footer must
	// still detect the 3-tab form (Test scope / Daemon / Submit).
	tabs, ok := MultiTabForm(mcqTabFrameV2)
	if !ok || tabs != 3 {
		t.Fatalf("MultiTabForm(v2 footer) = (%d,%v), want (3,true)", tabs, ok)
	}
}

func TestMultiTabFormCountsAnsweredTabs(t *testing.T) {
	// A partially-answered form marks answered tabs ☒ (not ☐). All three tabs
	// must still be counted (verified live: this read as 2 before the fix).
	frame := "←  ☒ Agent identity  ☐ Stats to show  ✔ Submit  →\n\n" +
		"Which stats?\n❯ 1. [ ] Auto-sends\n  2. [ ] Escalations\n\nEnter to select · Tab/Arrow keys to navigate · Esc to cancel\n"
	tabs, ok := MultiTabForm(frame)
	if !ok || tabs != 3 {
		t.Fatalf("MultiTabForm(answered tab) = (%d,%v), want (3,true)", tabs, ok)
	}
}

func TestMultiTabFormRejectsSingleQuestionForm(t *testing.T) {
	// The single-question AskUserQuestion form has the "Enter to select"
	// footer but no tab header and an ↑/↓ (not Tab/Arrow) footer.
	single := "How do you want to submit?\n❯ 1. All 4\n  2. Hold\n\nEnter to select · ↑/↓ to navigate · Esc to cancel\n"
	if tabs, ok := MultiTabForm(single); ok {
		t.Fatalf("single-question form must not detect as multi-tab, got %d tabs", tabs)
	}
}

// mcqSubmitFrame is the FINAL tab of a live multi-tab form: the header is
// intact but the tab-navigation footer is gone, replaced by the Submit
// confirmation body (issue #95). MultiTabForm must still detect it so the
// daemon's sweep does not abort on the last tab.
const mcqSubmitFrame = `⏺ The advisor confirmed the approach.
───────────────────────────────
←  ☐ Agent identity  ☐ Stats to show  ✔ Submit  →

Review your answers

⚠ You have not answered all questions

Ready to submit your answers?

❯ 1. Submit answers
  2. Cancel
`

func TestMultiTabFormDetectsFooterlessSubmitTab(t *testing.T) {
	// Regression for #95: the Submit tab drops the footer but keeps the
	// header; it must still resolve to the same 3-tab form.
	tabs, ok := MultiTabForm(mcqSubmitFrame)
	if !ok || tabs != 3 {
		t.Fatalf("MultiTabForm(submit tab) = (%d,%v), want (3,true)", tabs, ok)
	}
}

func TestMultiTabFormRejectsNarratedCheckboxes(t *testing.T) {
	// A narrated checkbox list without the navigation footer must not
	// trigger the sweep protocol.
	narrated := "Plan status:\n←  ☐ step one  ✔ done  →\nall good\n"
	if _, ok := MultiTabForm(narrated); ok {
		t.Fatal("checkbox narration without the Tab/Arrow footer must not detect as multi-tab")
	}
}

// mcqOneQuestionTabFrame is a LIVE capture (2026-07-20), trimmed from
// internal/classify/testdata/transcripts/choice_claude_mcq_tabs_one_question.txt
// (the byte-exact transcript — keep the two in step, they are not identical):
// a Claude AskUserQuestion form with exactly ONE question tab plus Submit,
// whose options are MULTI-select (`[ ]` checkboxes). With no sibling question
// to switch to, the footer carries no tab hint — it is the plain
// single-question footer — so the tab header was ignored and the form was
// answered as a plain menu: hap sent the digit alone, which only toggled the
// option's checkbox, and the agent stayed blocked on the Submit tab.
const mcqOneQuestionTabFrame = `● I'll explore how the task source parser works today.
────────────────────────────────────────────────────
Planning: /root/.claude/plans/make-sure-the-task-source.md
────────────────────────────────────────────────────
←  ☐ Scope  ✔ Submit  →

Parsing already works — I verified this exact file shape parses cleanly. The real gaps are addressing and context. Which should I fix?

❯ 1. [ ] Hierarchical task IDs (Recommended)
  ` + "`hap task <agent> done 3.4`" + ` currently errors (Atoi fails), and ` + "`done 3`" + ` silently marks positional item #3 — the wrong task.
  2. [ ] Section heading context
  Add a ` + "`{task_section}`" + ` template placeholder carrying the item's nearest ` + "`##`" + ` heading.
  3. [ ] Regression test only
  Add this exact tasks.md as a testdata fixture with a test pinning parse/numbering/next-task behavior.
  4. [ ] Show IDs in ` + "`hap task list`" + `
  Render the positional number alongside the file's own ID.
  5. [ ] Type something
     Submit
────────────────────────────────────────────────────
  6. Chat about this

Enter to select · ↑/↓ to navigate · Esc to cancel
`

// mcqOneQuestionSingleChoiceTabFrame is the other one-question shape: the same
// header + plain footer, but SINGLE-select options (no `[ ]` checkboxes). Both
// shapes must detect identically — the checkbox only decides how a tab is
// answered (toggle vs. pick), never whether the form is a tab form.
const mcqOneQuestionSingleChoiceTabFrame = `● Ready for your call.
────────────────────────────────────────────────────
←  ☐ Rollout  ✔ Submit  →

Which rollout order should I take?

❯ 1. Ship the parser fix first (Recommended)
     Smallest diff, unblocks the rest.
  2. Ship the CLI change first
     Bigger blast radius.
  3. Type something
     Submit
────────────────────────────────────────────────────
  4. Chat about this

Enter to select · ↑/↓ to navigate · Esc to cancel
`

func TestMultiTabFormDetectsOneQuestionTabForm(t *testing.T) {
	// A one-question form renders the tab header but the plain selection
	// footer. Both select kinds must resolve to their 2 tabs (question +
	// Submit) so the verified tab deliverer — not blind digit delivery —
	// answers them.
	cases := []struct {
		name        string
		frame       string
		multiSelect bool
	}{
		{"multi-choice (checkbox options)", mcqOneQuestionTabFrame, true},
		{"single-choice (plain options)", mcqOneQuestionSingleChoiceTabFrame, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tabs, ok := MultiTabForm(tc.frame)
			if !ok || tabs != 2 {
				t.Fatalf("MultiTabForm = (%d,%v), want (2,true)", tabs, ok)
			}
			// The select kind drives delivery: a checkbox tab toggles with the
			// digit and needs Enter to commit, a plain tab commits on the digit.
			if got := MultiSelectTab(ExtractMCQForm(tc.frame)); got != tc.multiSelect {
				t.Errorf("MultiSelectTab = %v, want %v", got, tc.multiSelect)
			}
		})
	}
}

func TestClaudeTabFormReadsOneQuestionSingleChoiceTabForm(t *testing.T) {
	state, ok := ClaudeTabForm(mcqOneQuestionSingleChoiceTabFrame)
	if !ok {
		t.Fatal("ClaudeTabForm(one-question single-choice form) = not a form")
	}
	if state.Kind != MCQClaudeTabs || state.AnswerCount != 2 {
		t.Errorf("kind/answers = %q/%d, want %q/2", state.Kind, state.AnswerCount, MCQClaudeTabs)
	}
	if state.Unanswered != 1 {
		t.Errorf("Unanswered = %d, want 1", state.Unanswered)
	}
	if state.SelectedOption != "1" {
		t.Errorf("SelectedOption = %q, want \"1\"", state.SelectedOption)
	}
	if !strings.Contains(state.Question, "Which rollout order") {
		t.Errorf("Question = %q, want the tab's question line", state.Question)
	}
}

func TestMultiTabFormDetectsAnsweredOneQuestionForm(t *testing.T) {
	// Verified live (2026-07-20, Claude Code v2.1.215): ticking the first
	// checkbox flips the header to ☒ while the form is STILL STANDING and
	// still owed an answer, and a checked box renders `[✔]`. Delivery re-reads
	// between keystrokes, so losing the form here stranded it mid-answer with
	// one box ticked.
	answered := "←  ☒ Shape  ✔ Submit  →\n\nWhich shapes do you like?\n\n" +
		"❯ 1. [✔] Circle\n  2. [ ] Square\n  3. [ ] Triangle\n\n" +
		"Enter to select · ↑/↓ to navigate · Esc to cancel\n"
	tabs, ok := MultiTabForm(answered)
	if !ok || tabs != 2 {
		t.Fatalf("MultiTabForm(mid-answer one-question form) = (%d,%v), want (2,true)", tabs, ok)
	}
	if !MultiSelectTab(ExtractMCQForm(answered)) {
		t.Error("a `[✔]`-checked option must still read as multi-select")
	}
	if states := OptionCheckStates(ExtractMCQForm(answered)); !states["1"] || states["2"] {
		t.Errorf("check states = %v, want option 1 checked", states)
	}
}

func TestMCQTabHeaderLine(t *testing.T) {
	// The header alone, uncorroborated: it must find the LIVE (last) header,
	// and report absence rather than an empty match.
	line, ok := MCQTabHeaderLine(mcqOneQuestionTabFrame)
	if !ok || !strings.Contains(line, "☐ Scope") {
		t.Errorf("MCQTabHeaderLine = (%q,%v), want the live header", line, ok)
	}
	stale := "←  ☒ Old  ✔ Submit  →\nsubmitted\n\n←  ☐ New  ✔ Submit  →\n"
	if line, _ := MCQTabHeaderLine(stale); !strings.Contains(line, "☐ New") {
		t.Errorf("MCQTabHeaderLine = %q, want the LAST header", line)
	}
	if _, ok := MCQTabHeaderLine("no form here\n1. Yes\n"); ok {
		t.Error("MCQTabHeaderLine reported a header on a pane without one")
	}
}

func TestMultiTabFormRejectsStaleHeaderAboveLivePlainMenu(t *testing.T) {
	// A finished form's header can sit in scrollback above a live plain menu.
	// Borrowing that menu's footer would route it into the Right-arrow sweep,
	// where a digit COMMITS — so the header needs its own live-form evidence:
	// no unanswered ☐, and the region below it shows no checkbox options.
	pane := "←  ☒ Scope  ✔ Submit  →\n\nAnswers submitted.\n\n" +
		"How do you want to submit?\n❯ 1. All 4\n  2. Hold\n\n" +
		"Enter to select · ↑/↓ to navigate · Esc to cancel\n"
	if tabs, ok := MultiTabForm(pane); ok {
		t.Fatalf("stale header above a live plain menu must not detect as multi-tab, got %d tabs", tabs)
	}
}

func TestMultiTabFormRejectsSelectionFooterAboveTheHeader(t *testing.T) {
	// The corroborating footer must sit BELOW the live header. A pane whose
	// only selection footer belongs to an earlier render is not evidence that
	// the trailing header is live.
	pane := "How do you want to submit?\n❯ 1. All 4\n  2. Hold\n\n" +
		"Enter to select · ↑/↓ to navigate · Esc to cancel\n\n" +
		"←  ☐ Scope  ✔ Submit  →\n"
	if tabs, ok := MultiTabForm(pane); ok {
		t.Fatalf("footer above the header must not corroborate it, got %d tabs", tabs)
	}
}

func TestParseMCQFormRoutesOneQuestionTabFormToTabs(t *testing.T) {
	state, ok := ParseMCQForm("claude", mcqOneQuestionTabFrame)
	if !ok {
		t.Fatal("ParseMCQForm(claude, one-question tab form) = not a form")
	}
	if state.Kind != MCQClaudeTabs {
		t.Errorf("Kind = %q, want %q", state.Kind, MCQClaudeTabs)
	}
	if state.AnswerCount != 2 {
		t.Errorf("AnswerCount = %d, want 2", state.AnswerCount)
	}
}

func TestClaudeTabFormReadsOneQuestionTabForm(t *testing.T) {
	state, ok := ClaudeTabForm(mcqOneQuestionTabFrame)
	if !ok {
		t.Fatal("ClaudeTabForm(one-question tab form) = not a form")
	}
	if state.Unanswered != 1 {
		t.Errorf("Unanswered = %d, want 1", state.Unanswered)
	}
	if state.SelectedOption != "1" {
		t.Errorf("SelectedOption = %q, want \"1\"", state.SelectedOption)
	}
	if !strings.Contains(state.Question, "Which should I fix?") {
		t.Errorf("Question = %q, want the tab's question line", state.Question)
	}
	// The options carry `[ ]` checkboxes: the digit toggles, Enter commits.
	// Delivery must treat this tab as multi-select.
	if !MultiSelectTab(ExtractMCQForm(mcqOneQuestionTabFrame)) {
		t.Error("MultiSelectTab = false, want true for a checkbox question")
	}
}

func TestClaudeMCQForm(t *testing.T) {
	singleQuestion := "How do you want to submit?\n❯ 1. All 4\n  2. Hold\n\nEnter to select · ↑/↓ to navigate · Esc to cancel\n"
	cases := []struct {
		name string
		pane string
		want bool
	}{
		{"multi-tab v1 footer", mcqTabFrame, true},
		{"multi-tab v2 footer", mcqTabFrameV2, true},
		{"multi-tab submit tab no footer", mcqSubmitFrame, true},
		{"single-question footer", singleQuestion, true},
		{"narrated checkbox no footer", "Plan status:\n←  ☐ step one  ✔ done  →\nall good\n", false},
		{"submit prompt without tab header", "All done.\nReady to submit your answers?\nyes I think so\n", false},
		{"submit narration mid-line with header", "←  ☐ a  ☐ b  ✔ Submit  →\nSo, are you ready to submit your answers? Not yet.\n", false},
		{"plain numbered list", "Summary:\n1. Refactored the consumer\n2. Updated the spec\n", false},
		{"narrated enter to select without nav tail", "run help: press Enter to select an entry\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClaudeMCQForm(tc.pane); got != tc.want {
				t.Errorf("ClaudeMCQForm = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseDigitSeries(t *testing.T) {
	cases := []struct {
		in   string
		want int
		ok   bool
	}{
		{"1 2 3 2 1", 5, true},
		{" 1  2 ", 2, true},
		{"1 1,3 2", 3, true}, // a multi-select tab toggles several options
		{"1,2 3", 2, true},   // comma group in the first tab
		{"1", 0, false},      // single digit = ordinary single-menu answer
		{"1 2 x", 0, false},  // non-digit token
		{"12 3", 0, false},   // multi-digit token is not a menu digit
		{"1, 3", 0, false},   // trailing comma is not a valid group
		{"1,0 3", 0, false},  // 0 is not a 1-based menu digit
		{"", 0, false},
		{"yes no", 0, false},
	}
	for _, c := range cases {
		seq, ok := ParseDigitSeries(c.in)
		if ok != c.ok || len(seq) != c.want {
			t.Errorf("ParseDigitSeries(%q) = (%v,%v), want len %d ok %v", c.in, seq, ok, c.want, c.ok)
		}
	}
}

func TestParseTabSelections(t *testing.T) {
	cases := []struct {
		in   string
		want [][]string
		ok   bool
	}{
		{"1 2 1", [][]string{{"1"}, {"2"}, {"1"}}, true},
		{"1 1,3 2", [][]string{{"1"}, {"1", "3"}, {"2"}}, true},
		{"1,3,3 2", [][]string{{"1", "3"}, {"2"}}, true}, // duplicate toggle deduped
		{"1", nil, false},    // single token is not a series
		{"1 x", nil, false},  // invalid token
		{"1, 2", nil, false}, // trailing comma
	}
	for _, c := range cases {
		got, ok := ParseTabSelections(c.in)
		if ok != c.ok {
			t.Errorf("ParseTabSelections(%q) ok = %v, want %v", c.in, ok, c.ok)
			continue
		}
		if ok && !reflect.DeepEqual(got, c.want) {
			t.Errorf("ParseTabSelections(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestExtractMCQFormDropsScrollback(t *testing.T) {
	got := ExtractMCQForm(mcqTabFrame)
	if strings.Contains(got, "Exploration done") {
		t.Errorf("scrollback above the form must be dropped, got %q", got)
	}
	if !strings.Contains(got, "New name for the") || !strings.Contains(got, "3. escalate_always") {
		t.Errorf("question and options must be kept, got %q", got)
	}
	if strings.Contains(got, "Enter to select") {
		t.Errorf("navigation footer must be dropped, got %q", got)
	}
}

func TestAggregateMCQFrames(t *testing.T) {
	agg := AggregateMCQFrames([]string{mcqTabFrame, mcqTabFrame, mcqTabFrame})
	for _, want := range []string{"[question 1/3]", "[question 2/3]", "[question 3/3]"} {
		if !strings.Contains(agg, want) {
			t.Errorf("aggregate missing %q", want)
		}
	}
}

func TestDecideMultiTabSeriesRuleFires(t *testing.T) {
	// A graduated digit-series rule fires when its length matches the tab
	// count; the series is never in the option set (FR-013 bypass).
	in := autonomous(baseInput(SituationChoice),
		"1 2 3 2 1", "1 2 3 2 1", "1 2 3 2 1", "1 2 3 2 1",
		"1 2 3 2 1", "1 2 3 2 1", "1 2 3 2 1", "1 2 3 2 1")
	in.Situation.TabCount = 5
	in.Situation.Options = []string{"never_auto (Recommended)", "deny"}
	d := Decide(in)
	if d.Action != ActionSend || d.Input != "1 2 3 2 1" {
		t.Fatalf("series rule should act, got %+v", d)
	}
}

func TestDecideMultiTabSeriesLengthMismatchEscalates(t *testing.T) {
	// A learned 4-digit series against a 5-tab form must never partially
	// answer: escalate as an unfamiliar option set.
	in := autonomous(baseInput(SituationChoice),
		"1 2 3 2", "1 2 3 2", "1 2 3 2", "1 2 3 2",
		"1 2 3 2", "1 2 3 2", "1 2 3 2", "1 2 3 2")
	in.Situation.TabCount = 5
	d := Decide(in)
	if d.Action != ActionEscalate || d.Reason != ReasonUnfamiliarOptions {
		t.Fatalf("length mismatch must escalate, got %+v", d)
	}
}

func TestDecideMultiTabSingleAnswerEscalates(t *testing.T) {
	// A plain learned option ("never_auto") cannot answer a multi-tab form.
	in := autonomous(baseInput(SituationChoice),
		"never_auto", "never_auto", "never_auto", "never_auto",
		"never_auto", "never_auto", "never_auto", "never_auto")
	in.Situation.TabCount = 5
	in.Situation.Options = []string{"never_auto", "deny"}
	d := Decide(in)
	if d.Action != ActionEscalate || d.Reason != ReasonUnfamiliarOptions {
		t.Fatalf("single answer on a multi-tab form must escalate, got %+v", d)
	}
}

func TestIrreversibleScanCoversWholeAggregate(t *testing.T) {
	// The FR-016 heuristic normally scans only the tail window; a swept
	// aggregate is ALL actionable region, so destructive phrasing in the
	// FIRST question must stay inside the scan even when later questions
	// push it past the tail window.
	first := "[question 1/5]\nShould I run the irreversible cleanup — no undo — on the data dir?\n❯ 1. Yes\n  2. No\n"
	var later strings.Builder
	later.WriteString(first)
	for q := 2; q <= 5; q++ {
		later.WriteString("\n\n[question " + string(rune('0'+q)) + "/5]\n")
		for i := 0; i < 15; i++ {
			later.WriteString("harmless filler line about formatting preferences\n")
		}
	}
	s := Situation{Type: SituationChoice, AgentType: "claude", TabCount: 5, Content: later.String()}
	if scan := IrreversibleScanContent(s, ""); !strings.Contains(scan, "no undo") {
		t.Fatal("aggregate scan must include the first question's destructive phrasing")
	}
	// Single-question forms keep the scoped tail window (FR-016: narration
	// about destructive ops must not be flagged perpetually).
	s.TabCount = 0
	if scan := IrreversibleScanContent(s, ""); strings.Contains(scan, "no undo") {
		t.Fatal("single-question scan should keep the tail window scoping")
	}
}

// claudePreviewFrame is a real capture (audit #671, reproduced live 2026-07-16)
// of the PREVIEW rendering: option list left, preview box right, "Notes: press
// n to add notes", and an unnumbered "Chat about this". On this rendering a
// digit only moves the caret — Enter commits.
const claudePreviewFrame = `←  ☐ Shape  ☐ Speed  ✔ Submit  →

Which shape?

❯ 1. Circle                       ┌──────────────┐
  2. Square                       │ ****         │
                                  └──────────────┘

                                  Notes: press n to add notes

  Chat about this

Enter to select · ↑/↓ to navigate · n to add notes · Tab to switch questions · Esc to cancel`

// claudePlainFrame is the PLAIN rendering (audit #674): plain numbered options,
// no preview box. A digit selects and commits here.
const claudePlainFrame = `←  ☒ Color  ☐ Size  ✔ Submit  →

Which size?

  1. Small
     Small
❯ 2. Large
     Large
  3. Type something.

Enter to select · ↑/↓ to navigate · Tab to switch questions · Esc to cancel`

// claudeSubmitFrame is the final Submit tab, which drops the navigation footer
// (issue #95) and carries the confirmation body instead.
const claudeSubmitFrame = `←  ☒ Shape  ☒ Speed  ✔ Submit  →

Review your answers

Ready to submit your answers?

❯ 1. Submit answers
  2. Cancel`

func TestClaudeTabForm(t *testing.T) {
	tests := []struct {
		name           string
		pane           string
		wantOK         bool
		wantCount      int
		wantUnanswered int
		wantCaret      string
	}{
		{
			name: "preview tab: both questions unanswered, caret on option 1",
			pane: claudePreviewFrame, wantOK: true,
			wantCount: 3, wantUnanswered: 2, wantCaret: "1",
		},
		{
			name: "plain tab: one answered (☒), caret moved to option 2",
			pane: claudePlainFrame, wantOK: true,
			wantCount: 3, wantUnanswered: 1, wantCaret: "2",
		},
		{
			name: "submit tab: all answered, footer-less confirmation body",
			pane: claudeSubmitFrame, wantOK: true,
			wantCount: 3, wantUnanswered: 0, wantCaret: "1",
		},
		{
			name:   "not a multi-tab form",
			pane:   "just some agent narration\n1. not a form",
			wantOK: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ClaudeTabForm(tc.pane)
			if ok != tc.wantOK {
				t.Fatalf("ClaudeTabForm ok = %v, want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if got.Kind != MCQClaudeTabs {
				t.Errorf("Kind = %q, want %q", got.Kind, MCQClaudeTabs)
			}
			if got.AnswerCount != tc.wantCount {
				t.Errorf("AnswerCount = %d, want %d", got.AnswerCount, tc.wantCount)
			}
			if got.Unanswered != tc.wantUnanswered {
				t.Errorf("Unanswered = %d, want %d", got.Unanswered, tc.wantUnanswered)
			}
			if got.SelectedOption != tc.wantCaret {
				t.Errorf("SelectedOption = %q, want %q", got.SelectedOption, tc.wantCaret)
			}
		})
	}
}

// The composer line ("❯ " + typed text) and stale scrollback renders must never
// supply the caret — only the LAST live form does.
func TestClaudeTabFormIgnoresComposerAndStaleRenders(t *testing.T) {
	pane := "❯ 9. an old stale render\n\n" + claudePreviewFrame + "\n\n❯ what should I do next?\n"
	got, ok := ClaudeTabForm(pane)
	if !ok {
		t.Fatal("ClaudeTabForm should still parse the live form")
	}
	if got.SelectedOption != "1" {
		t.Errorf("SelectedOption = %q, want %q (the live form's caret)", got.SelectedOption, "1")
	}
	if got.Unanswered != 2 {
		t.Errorf("Unanswered = %d, want 2", got.Unanswered)
	}
}
