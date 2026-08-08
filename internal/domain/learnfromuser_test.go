package domain

import "testing"

// TestLearnRequestSpellsOutSentinels: hap's internal tokens must never reach a
// learn prompt verbatim. A model has no way to know "@next_task:declared" is a
// token rather than a literal string the operator typed, so the lesson it
// records would be about the spelling instead of the behavior.
func TestLearnRequestSpellsOutSentinels(t *testing.T) {
	cases := []struct {
		name             string
		suggestion       string
		correction       string
		wantSuggestion   string
		wantCorrection   string
		wantUnchangedSug bool
	}{
		{
			name: "ordinary text passes through untouched",
			// The common case, and the one a broad rewrite would break.
			suggestion: "Yes", correction: "No, use --dry-run",
			wantSuggestion: "Yes", wantCorrection: "No, use --dry-run",
		},
		{
			name:       "noop sentinel",
			suggestion: ActionNoop, correction: ActionNoop,
			wantSuggestion: ActionNoopSuggestion, wantCorrection: ActionNoopSuggestion,
		},
		{
			name:       "bare noop spellings normalize too",
			suggestion: "no-op", correction: "NOOP",
			wantSuggestion: ActionNoopSuggestion, wantCorrection: ActionNoopSuggestion,
		},
		{
			name:       "declared-task sentinel",
			suggestion: ActionNextDeclaredTask, correction: ActionNextDeclaredTask,
			wantSuggestion: "send the agent its next task from its declared task list",
			wantCorrection: "send the agent its next task from its declared task list",
		},
		{
			name:       "inferred-task sentinel",
			suggestion: ActionNextInferredTask, correction: ActionNextInferredTask,
			wantSuggestion: "send the agent the next task inferred from its own on-screen todo list",
			wantCorrection: "send the agent the next task inferred from its own on-screen todo list",
		},
		{
			name: "text merely CONTAINING a sentinel is not rewritten",
			// Only a bare sentinel is a token; an operator writing about one is
			// writing prose, and mangling it would corrupt the lesson.
			suggestion: "reply @noop only when idle", correction: "never send @next_task:declared here",
			wantSuggestion: "reply @noop only when idle", wantCorrection: "never send @next_task:declared here",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := LearnRequest{Suggestion: tc.suggestion, Correction: tc.correction}
			if got := r.SuggestionText(); got != tc.wantSuggestion {
				t.Errorf("SuggestionText() = %q, want %q", got, tc.wantSuggestion)
			}
			if got := r.CorrectionText(); got != tc.wantCorrection {
				t.Errorf("CorrectionText() = %q, want %q", got, tc.wantCorrection)
			}
		})
	}
}

// TestLearnRequestMissingSuggestion: an escalation hap had no opinion on still
// teaches, so {suggestion} must render as a readable fact rather than as the
// empty string, which would leave the shipped prompt's "You were about to
// answer:" trailing into nothing.
func TestLearnRequestMissingSuggestion(t *testing.T) {
	for _, empty := range []string{"", "   ", "\n\t"} {
		r := LearnRequest{Suggestion: empty}
		if got := r.SuggestionText(); got != NoSuggestionText {
			t.Errorf("SuggestionText(%q) = %q, want NoSuggestionText", empty, got)
		}
	}
}
