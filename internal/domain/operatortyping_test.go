package domain

import "testing"

// claudeDraft is claudeComposer with a half-written operator message on the
// caret line — the state #526 saw twice in one day, where an automatic send
// would have been appended to the draft and submitted with it.
const claudeDraft = "" +
	"● Rebased the branch onto main and re-ran the suite.\n" +
	"\n" +
	"────────────────────────────────────────────────────────────────────────\n" +
	"❯ can you also check whether the\n" +
	"────────────────────────────────────────────────────────────────────────\n" +
	"  tmp | Fable 5 (0%) | default | 09acb1fa\n"

func TestOperatorTypingReadsAClaudeDraft(t *testing.T) {
	typing, known := OperatorTyping("claude", claudeDraft)
	if !known || !typing {
		t.Fatalf("typing=%v known=%v, want a draft to be seen", typing, known)
	}
}

// TestOperatorTypingIsFalseAtAnEmptyComposer is the control: without it a
// predicate answering "typing" for every recognised composer would pass above
// and withhold every hand-out from every agent.
func TestOperatorTypingIsFalseAtAnEmptyComposer(t *testing.T) {
	typing, known := OperatorTyping("claude", claudeComposer)
	if !known {
		t.Fatal("a proven composer must be a known answer")
	}
	if typing {
		t.Fatal("an empty composer read as a draft")
	}
}

// TestOperatorTypingIsUnknownWithoutAComposer pins the rule the whole predicate
// rests on. The daemon's classification read is a CONSUMING delta that
// routinely returns no footer, so "no composer" must be UNKNOWN — a caller that
// suppressed on it would suppress on nearly every event.
func TestOperatorTypingIsUnknownWithoutAComposer(t *testing.T) {
	for name, pane := range map[string]string{
		"empty capture":     "",
		"transcript only":   "● Ran the tests. All 23 packages pass.\n",
		"standing approval": claudePlanApproval,
	} {
		t.Run(name, func(t *testing.T) {
			typing, known := OperatorTyping("claude", pane)
			if known {
				t.Fatalf("typing=%v known=true, want unknown", typing)
			}
		})
	}
}

func TestOperatorTypingIsGatedOnAgentType(t *testing.T) {
	for _, agentType := range []string{"", "unknown-cli"} {
		if _, known := OperatorTyping(agentType, claudeDraft); known {
			t.Fatalf("agent type %q claimed to read Claude's composer", agentType)
		}
	}
}

// TestOperatorTypingRefusesToGuessForCodex documents a deliberate refusal, not
// a gap: codex renders suggestion text on the caret line of an EMPTY composer,
// and this package has no sample separating that from a draft. Reading the
// placeholder as a draft would withhold every hand-out from every parked codex
// agent, silently.
func TestOperatorTypingRefusesToGuessForCodex(t *testing.T) {
	pane := "─ Worked for 10m 49s ────────────────\n" +
		"\n" +
		"› Summarize recent commits\n" +
		"\n" +
		"  gpt-5.6-sol high · /workspaces/herdr-auto-pilot\n"
	if _, known := OperatorTyping("codex", pane); known {
		t.Fatal("codex answered a question it has no evidence for")
	}
}

// agyPane renders the composer sandwich and status bar at the tail, which is
// the only place AgyComposerVisible will look.
func agyPane(caret, leftToken string) string {
	return "● Ran the build.\n" +
		"────────────────────────────────────────────────────────────\n" +
		caret + "\n" +
		"────────────────────────────────────────────────────────────\n" +
		leftToken + "                                        Gemini 3.8 Flash · high\n"
}

// TestOperatorTypingReadsAnAgyDraft covers why AgyComposerReady could not
// simply be negated: its false folds a draft together with a modal, the survey
// and a working turn, and only one of those is a human at the keyboard.
func TestOperatorTypingReadsAnAgyDraft(t *testing.T) {
	// A draft replaces the bar's "? for shortcuts" token with "esc to cancel",
	// which is precisely why AgyComposerReady says no — and says no to three
	// other states as well.
	typing, known := OperatorTyping("agy", agyPane("> can you also check", "esc to cancel"))
	if !known || !typing {
		t.Fatalf("typing=%v known=%v, want a draft to be seen", typing, known)
	}
}

func TestOperatorTypingIsFalseAtAnEmptyAgyComposer(t *testing.T) {
	for name, caret := range map[string]string{
		"bare caret":       ">",
		"mode placeholder": "> Plan mode: research & plan only (shift+tab to cycle)",
	} {
		t.Run(name, func(t *testing.T) {
			typing, known := OperatorTyping("agy", agyPane(caret, "? for shortcuts"))
			if !known {
				t.Fatal("a proven agy composer must be a known answer")
			}
			if typing {
				t.Fatal("an empty agy composer read as a draft")
			}
		})
	}
}

// TestOperatorTypingIsUnknownUnderAnAgyModal is the safety control. herdr
// reports every agy modal as idle, so a predicate that read the top line of a
// standing form as a draft — or as an empty composer — would be answering about
// the wrong screen entirely. Neither: UNKNOWN.
func TestOperatorTypingIsUnknownUnderAnAgyModal(t *testing.T) {
	pane := "● Bash(rm -rf build/)\n" +
		"  Do you want to allow this?\n" +
		"  1. Yes\n" +
		"  2. No\n" +
		"esc to cancel                                Gemini 3.8 Flash · high\n"
	if _, known := OperatorTyping("agy", pane); known {
		t.Fatal("a modal was read as a composer")
	}
}
