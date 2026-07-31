package domain

import "testing"

func TestEscalationReasonTag(t *testing.T) {
	tests := []struct {
		name      string
		rationale string
		want      string
	}{
		{
			name:      "bare tag",
			rationale: "[no_task_source]",
			want:      "no_task_source",
		},
		{
			name:      "tag with diagnostic",
			rationale: `[never_auto_match] pattern (?i)\brm\s+-rf\b matched "rm -rf build" (source=seed)`,
			want:      "never_auto_match",
		},
		{
			name:      "no tag",
			rationale: "contradictory history",
			want:      "",
		},
		{
			// Prose that merely opens with a bracket is not a reason token.
			// Reason tags are machine-written and never contain spaces, so the
			// operator is told "escalated by the X control" only when X is real.
			name:      "prose in brackets",
			rationale: "[the agent said something odd] and then stopped",
			want:      "",
		},
		{
			name:      "empty brackets",
			rationale: "[] nothing",
			want:      "",
		},
		{
			name:      "unterminated bracket",
			rationale: "[never_auto_match",
			want:      "",
		},
		{
			name:      "empty rationale",
			rationale: "",
			want:      "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := EscalationReasonTag(tc.rationale); got != tc.want {
				t.Errorf("EscalationReasonTag(%q) = %q, want %q", tc.rationale, got, tc.want)
			}
		})
	}
}

func TestEscalationReasonTagCoversEveryUnconfirmableVeto(t *testing.T) {
	// The refusal message names the control that fired, so the tag every veto
	// actually stamps must parse. A renamed reason silently falling back to the
	// generic wording is exactly the regression this catches.
	for _, r := range []EscalateReason{
		ReasonDaemonPaused, ReasonUnclassifiable, ReasonOverMasked, ReasonNeverAutoMatch,
	} {
		if got := EscalationReasonTag("[" + string(r) + "] some diagnostic"); got != string(r) {
			t.Errorf("reason %q does not round-trip through the tag parser, got %q", r, got)
		}
	}
}
