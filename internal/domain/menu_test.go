package domain

import "testing"

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
		name    string
		sitType SituationType
		content string
		chosen  string
		want    string
		mapped  bool
	}{
		{"approval label maps to digit", SituationApproval, menu, "Yes", "1", true},
		{"choice numeric selection maps", SituationChoice, menu, "2", "2", true},
		{"approval free text without menu stays literal", SituationApproval, "Enter a message:", "looks good", "looks good", false},
		{"idle never maps even over a numbered list", SituationIdle, "done:\n1. ran tests\n2. built", "continue with the plan", "continue with the plan", false},
		{"error retry command stays literal", SituationError, menu, "go test ./...", "go test ./...", false},
		{"Codex rate-limit error option maps", SituationError, codexRateLimitFrame, "Keep current model", "2", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, mapped := DeliverOutbound(tc.sitType, tc.content, tc.chosen)
			if got != tc.want || mapped != tc.mapped {
				t.Errorf("DeliverOutbound(%v, %q) = (%q, %v), want (%q, %v)",
					tc.sitType, tc.chosen, got, mapped, tc.want, tc.mapped)
			}
			// DeliverKeystroke must stay in lockstep with DeliverOutbound.
			if ks := DeliverKeystroke(tc.sitType, tc.content, tc.chosen); ks != got {
				t.Errorf("DeliverKeystroke = %q, DeliverOutbound text = %q", ks, got)
			}
		})
	}
}
