package domain

import "testing"

func TestClaudeErrorForm(t *testing.T) {
	cases := []struct {
		name string
		pane string
		want bool
	}{
		{"limit reset am", "⎿ You've hit your limit · resets 1am\n", true},
		{"limit reset utc", "⎿  You've hit your limit · resets 6pm (UTC)\n", true},
		{"limit curly apostrophe", "You’ve hit your limit\n", true},
		{"limit usage qualifier", "you've hit your usage limit for today\n", true},
		{"interrupted prompt", "⎿  Interrupted · What should Claude do instead?\n", true},
		// Ordinary error-shaped narration must NOT match (the whole point of
		// the tightening).
		{"narrated build failure", "ERROR: build failed with exit code 1\nThe build failed. Retry, skip, or abort?\n", false},
		{"narrated stack trace", "panic: nil pointer\ngoroutine 1 [running]:\nmain.main()\n", false},
		{"narrated interrupt word", "the download was interrupted midway and resumed\n", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, got := ClaudeErrorForm(tc.pane)
			if got != tc.want {
				t.Errorf("ClaudeErrorForm ok = %v, want %v", got, tc.want)
			}
			if got != (kind != "") {
				t.Errorf("kind %q inconsistent with ok %v", kind, got)
			}
		})
	}
}

func TestClaudeErrorFormKind(t *testing.T) {
	if kind, _ := ClaudeErrorForm("You've hit your limit · resets 1am\n"); kind != ClaudeErrorLimit {
		t.Errorf("limit kind = %q, want %q", kind, ClaudeErrorLimit)
	}
	if kind, _ := ClaudeErrorForm("Interrupted · What should Claude do instead?\n"); kind != ClaudeErrorInterrupted {
		t.Errorf("interrupted kind = %q, want %q", kind, ClaudeErrorInterrupted)
	}
}
