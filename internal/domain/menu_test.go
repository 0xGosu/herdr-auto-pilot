package domain

import (
	"strings"
	"testing"
)

const claudeApproval = "Bash(go test ./...)\n\nDo you want to proceed?\n❯ 1. Yes\n  2. No, and tell the agent what to do differently\n"

// A multi-SELECT question renders per-option checkboxes; a single-select one
// renders plain numbered options.
const multiSelectFrame = "Which stats should show?\n❯ 1. [ ] Auto-sends\n  2. [x] Escalations\n  3. [ ] Confirmed\n\nEnter to select · Tab to switch questions\n"

func TestMultiSelectTab(t *testing.T) {
	if !MultiSelectTab(multiSelectFrame) {
		t.Error("checkbox options must classify as multi-select")
	}
	if MultiSelectTab(claudeApproval) {
		t.Error("plain numbered options must NOT classify as multi-select")
	}
}

func TestOptionCheckStates(t *testing.T) {
	states := OptionCheckStates(multiSelectFrame)
	if len(states) != 3 {
		t.Fatalf("want 3 checkbox states, got %d: %+v", len(states), states)
	}
	if states["1"] || !states["2"] || states["3"] {
		t.Errorf("check states wrong: %+v (want only option 2 checked)", states)
	}
	// A frame without checkboxes yields no states.
	if got := OptionCheckStates(claudeApproval); len(got) != 0 {
		t.Errorf("non-checkbox options must yield no check states, got %+v", got)
	}
}

func TestCheckedOutside(t *testing.T) {
	// multiSelectFrame has option 2 checked.
	if got := CheckedOutside(multiSelectFrame, nil); len(got) != 1 || got[0] != "2" {
		t.Errorf("CheckedOutside(nil) = %v, want [2]: with no answer decided, any checked box is foreign", got)
	}
	if got := CheckedOutside(multiSelectFrame, []string{"2"}); len(got) != 0 {
		t.Errorf("CheckedOutside(chosen 2) = %v, want none: this answer's own box is not foreign", got)
	}
	if got := CheckedOutside(multiSelectFrame, []string{"1", "2"}); len(got) != 0 {
		t.Errorf("CheckedOutside(chosen 1,2) = %v, want none", got)
	}
	if got := CheckedOutside(multiSelectFrame, []string{"1", "3"}); len(got) != 1 || got[0] != "2" {
		t.Errorf("CheckedOutside(chosen 1,3) = %v, want [2]", got)
	}
	if got := CheckedOutside(claudeApproval, nil); len(got) != 0 {
		t.Errorf("a frame without checkboxes has nothing checked, got %v", got)
	}
}

func TestClearCheckboxMarks(t *testing.T) {
	cleared := ClearCheckboxMarks(multiSelectFrame)
	for digit, checked := range OptionCheckStates(cleared) {
		if checked {
			t.Errorf("option %s is still checked after clearing: %q", digit, cleared)
		}
	}
	// Only the box changes: the option set and their labels survive.
	if len(OptionCheckStates(cleared)) != len(OptionCheckStates(multiSelectFrame)) {
		t.Error("clearing the marks must not drop options")
	}
	if before, after := ParseNumberedOptions(multiSelectFrame), ParseNumberedOptions(cleared); len(before) != len(after) {
		t.Fatalf("option count changed: %d -> %d", len(before), len(after))
	}
	// Two renders of one form differing only in what a partial delivery
	// toggled must compare equal.
	if ClearCheckboxMarks(multiSelectFrame) != ClearCheckboxMarks(cleared) {
		t.Error("normalized renders of the same form must compare equal")
	}
	// Prose is untouched — only numbered option lines carry a box.
	const prose = "the runbook says [x] means done\n"
	if got := ClearCheckboxMarks(prose); got != prose {
		t.Errorf("ClearCheckboxMarks rewrote non-option text: %q", got)
	}

	// The tab header's answered marks normalize too: ticking a box flips its
	// tab ☐ -> ☒ while the form still stands, so leaving the header alone
	// would keep two renders of one form comparing unequal — the exact case
	// the comparison exists to accept.
	const untouched = "←  ☐ Shape  ✔ Submit  →\n\nWhich?\n\n❯ 1. [ ] Circle\n  2. [ ] Square\n"
	const midAnswer = "←  ☒ Shape  ✔ Submit  →\n\nWhich?\n\n❯ 1. [✔] Circle\n  2. [ ] Square\n"
	if ClearCheckboxMarks(untouched) != ClearCheckboxMarks(midAnswer) {
		t.Errorf("a partially-answered render must normalize to its untouched form:\n%q\n%q",
			ClearCheckboxMarks(untouched), ClearCheckboxMarks(midAnswer))
	}
	// The Submit entry is not a question mark and must survive.
	if !strings.Contains(ClearCheckboxMarks(midAnswer), "✔ Submit") {
		t.Error("the ✔ Submit entry must not be normalized away")
	}
	// A genuinely different form still compares unequal.
	const other = "←  ☐ Shape  ✔ Submit  →\n\nWhich?\n\n❯ 1. [ ] Circle\n  2. [ ] Hexagon\n"
	if ClearCheckboxMarks(untouched) == ClearCheckboxMarks(other) {
		t.Error("different option text must not normalize to equal")
	}
}

func TestParseNumberedOptions(t *testing.T) {
	opts := ParseNumberedOptions(claudeApproval)
	if len(opts) != 2 {
		t.Fatalf("want 2 options, got %d: %+v", len(opts), opts)
	}
	if opts[0].Number != "1" || opts[0].Label != "Yes" {
		t.Errorf("option 1 = %+v", opts[0])
	}
	if opts[1].Number != "2" || opts[1].Label != "No, and tell the agent what to do differently" {
		t.Errorf("option 2 = %+v", opts[1])
	}
}

func TestMenuKeystroke(t *testing.T) {
	tests := []struct {
		name    string
		content string
		chosen  string
		want    string
		mapped  bool
	}{
		{"label to digit", claudeApproval, "Yes", "1", true},
		{"label case-insensitive", claudeApproval, "yes", "1", true},
		{"second option full label", claudeApproval, "No, and tell the agent what to do differently", "2", true},
		{"unique prefix abbreviation", claudeApproval, "No", "2", true},
		{"already a digit passes through", claudeApproval, "2", "2", true},
		{"bracketed menu", "Pick one:\n[1] Apply\n[2] Skip\n", "Skip", "2", true},
		{"paren menu", "1) Merge\n2) Rebase\n", "Rebase", "2", true},
		{"no menu → literal", "Enter your commit message:", "fix: the thing", "fix: the thing", false},
		{"menu but unmatched → literal", claudeApproval, "Maybe", "Maybe", false},
		{"digit out of range → literal", claudeApproval, "9", "9", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, mapped := MenuKeystroke(tc.content, tc.chosen)
			if got != tc.want || mapped != tc.mapped {
				t.Errorf("MenuKeystroke(%q) = (%q, %v), want (%q, %v)",
					tc.chosen, got, mapped, tc.want, tc.mapped)
			}
		})
	}
}

func TestMenuKeystrokeAmbiguousPrefixStaysLiteral(t *testing.T) {
	// Two options share a prefix: an abbreviation must NOT guess.
	content := "1. Yes, once\n2. Yes, always\n3. No\n"
	if got, mapped := MenuKeystroke(content, "Yes"); mapped {
		t.Errorf("ambiguous prefix must not map, got %q", got)
	}
	// The exact label still resolves.
	if got, mapped := MenuKeystroke(content, "Yes, always"); !mapped || got != "2" {
		t.Errorf("exact label = (%q, %v), want (2, true)", got, mapped)
	}
}

func TestDeliverOutbound(t *testing.T) {
	menu := "Allow this tool?\n❯ 1. Yes\n  2. No\n"
	tests := []struct {
		name      string
		sitType   SituationType
		agentType string
		content   string
		chosen    string
		want      string
		mapped    bool
	}{
		{"approval label maps to digit", SituationApproval, "claude", menu, "Yes", "1", true},
		{"choice numeric selection maps", SituationChoice, "claude", menu, "2", "2", true},
		{"approval free text without menu stays literal", SituationApproval, "claude", "Enter a message:", "looks good", "looks good", false},
		{"idle never maps even over a numbered list", SituationIdle, "codex", "done:\n1. ran tests\n2. built", "continue with the plan", "continue with the plan", false},
		{"error retry command stays literal", SituationError, "claude", menu, "go test ./...", "go test ./...", false},
		{"Codex rate-limit error option maps", SituationError, "codex", codexRateLimitFrame, "Keep current model", "2", true},
		{"non-Codex rate-limit-shaped error stays literal", SituationError, "claude", codexRateLimitFrame, "Keep current model", "Keep current model", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, mapped := DeliverOutbound(tc.sitType, tc.agentType, tc.content, tc.chosen)
			if got != tc.want || mapped != tc.mapped {
				t.Errorf("DeliverOutbound(%v, %q) = (%q, %v), want (%q, %v)",
					tc.sitType, tc.chosen, got, mapped, tc.want, tc.mapped)
			}
			// DeliverKeystroke must stay in lockstep with DeliverOutbound.
			if ks := DeliverKeystroke(tc.sitType, tc.agentType, tc.content, tc.chosen); ks != got {
				t.Errorf("DeliverKeystroke = %q, DeliverOutbound text = %q", ks, got)
			}
		})
	}
}
