package domain

import (
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
		{"1", 0, false},     // single digit = ordinary single-menu answer
		{"1 2 x", 0, false}, // non-digit token
		{"12 3", 0, false},  // multi-digit token is not a menu digit
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
