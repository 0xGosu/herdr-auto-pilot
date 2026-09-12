package domain

import "testing"

// agyShellApproval is agy 1.2.1's shell approval: one narrow grant, two
// widening grants, one refusal.
func agyShellApproval() AgyForm {
	return AgyForm{Kind: AgyFormApproval, Options: []NumberedOption{
		{Number: "1", Label: "Yes, run command"},
		{Number: "2", Label: "Yes, and always allow in this conversation for commands that start with 'go'"},
		{Number: "3", Label: "Yes, and always allow for commands that start with 'go' (Persist to settings.json)"},
		{Number: "4", Label: "No, cancel"},
	}}
}

func TestNarrowestAgyApprovalSubstitutesAWideningChoice(t *testing.T) {
	form := agyShellApproval()
	for _, chosen := range []string{
		"Yes, and always allow in this conversation for commands that start with 'go'",
		"Yes, and always allow for commands that start with 'go' (Persist to settings.json)",
		// The LLM answers an approval with select_options, so the action can
		// arrive as the option NUMBER rather than its label.
		"2",
		"3",
	} {
		got, swapped := NarrowestAgyApproval(form, chosen)
		if !swapped || got != "Yes, run command" {
			t.Errorf("NarrowestAgyApproval(%q) = %q, %v; want the narrow grant", chosen, got, swapped)
		}
	}
}

// The substitution must never change an answer that was already narrow, and
// above all must never turn an approval into a refusal.
func TestNarrowestAgyApprovalLeavesEveryOtherAnswerAlone(t *testing.T) {
	form := agyShellApproval()
	for _, chosen := range []string{"Yes, run command", "No, cancel", "1", "4"} {
		got, swapped := NarrowestAgyApproval(form, chosen)
		if swapped || got != chosen {
			t.Errorf("NarrowestAgyApproval(%q) = %q, %v; want it untouched", chosen, got, swapped)
		}
	}
	// A reply naming no option is left for the refusal gates that exist for it.
	if got, swapped := NarrowestAgyApproval(form, "Maybe"); swapped || got != "Maybe" {
		t.Errorf("an unmatched reply must pass through: %q %v", got, swapped)
	}
}

// With no single narrow grant there is nothing to prefer, and guessing between
// two would answer the prompt differently from anything a human reviewed.
func TestNarrowestAgyApprovalNeedsExactlyOneNarrowGrant(t *testing.T) {
	none := AgyForm{Kind: AgyFormApproval, Options: []NumberedOption{
		{Number: "1", Label: "Yes, and always allow commands starting with 'go'"},
		{Number: "2", Label: "No, cancel"},
	}}
	if got, swapped := NarrowestAgyApproval(none, "1"); swapped {
		t.Errorf("no narrow option exists, yet it substituted %q", got)
	}
	two := AgyForm{Kind: AgyFormApproval, Options: []NumberedOption{
		{Number: "1", Label: "Yes, run command"},
		{Number: "2", Label: "Yes, run command in a sandbox"},
		{Number: "3", Label: "Yes, and always allow commands starting with 'go'"},
		{Number: "4", Label: "No, cancel"},
	}}
	if got, swapped := NarrowestAgyApproval(two, "3"); swapped {
		t.Errorf("two narrow options are ambiguous, yet it chose %q", got)
	}
}

// Only approvals. A question form's options are answers to a question, not
// permission grants, and "always" in one of them means nothing of the sort.
func TestNarrowestAgyApprovalOnlyAppliesToApprovals(t *testing.T) {
	q := AgyForm{Kind: AgyFormQuestion, Options: []NumberedOption{
		{Number: "1", Label: "Yes, run command"},
		{Number: "2", Label: "Yes, and always allow it"},
	}}
	if _, swapped := NarrowestAgyApproval(q, "2"); swapped {
		t.Error("a question form must not be rewritten")
	}
}

func TestAgyWideningAndGrantingLabels(t *testing.T) {
	for label, want := range map[string]bool{
		"Yes, and always allow in this conversation":       true,
		"Yes, and always allow (Persist to settings.json)": true,
		"Yes, don't ask again":                             true,
		"Allow all":                                        true,
		"Yes, run command":                                 false,
		// A file prompt's plain grant contains "allow" and must NOT read as
		// widening — anchoring on the bare word would refuse the narrow answer.
		"Yes, allow access to this file": false,
		"No, cancel":                     false,
	} {
		if got := AgyWideningOption(label); got != want {
			t.Errorf("AgyWideningOption(%q) = %v, want %v", label, got, want)
		}
	}
	for label, want := range map[string]bool{
		"Yes, run command":               true,
		"Yes, allow access to this file": true,
		"Allow access":                   true,
		"No, cancel":                     false,
		"No, deny access to this file":   false,
		"Skip":                           false,
	} {
		if got := AgyGrantingOption(label); got != want {
			t.Errorf("AgyGrantingOption(%q) = %v, want %v", label, got, want)
		}
	}
}
