package domain

import "testing"

const claudeApproval = "Bash(go test ./...)\n\nDo you want to proceed?\n❯ 1. Yes\n  2. No, and tell the agent what to do differently\n"

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
