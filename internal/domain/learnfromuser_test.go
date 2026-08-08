package domain

import (
	"strings"
	"testing"
	"unicode/utf8"
)

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

// TestTailRunesNeverSplitsARune: the transcript is carried to an operator-facing
// row, so a cut in the middle of a multibyte character (rendering as U+FFFD) is
// a visible defect. It also must not allocate a rune slice of the whole input —
// that is why it slices bytes and aligns forward.
func TestTailRunesNeverSplitsARune(t *testing.T) {
	// Each "é" is two bytes, so most byte offsets land mid-rune.
	s := strings.Repeat("é", 50)
	for n := 1; n <= len(s); n++ {
		got := TailRunes(s, n)
		if !utf8.ValidString(got) {
			t.Fatalf("TailRunes(%d) produced invalid UTF-8: %q", n, got)
		}
		if strings.ContainsRune(got, utf8.RuneError) {
			t.Fatalf("TailRunes(%d) produced a replacement char: %q", n, got)
		}
		if n < len(s) && !strings.HasPrefix(got, "…") {
			t.Fatalf("TailRunes(%d) cut without marking it: %q", n, got)
		}
		// The ellipsis is added on top of the budget, so the kept tail itself
		// must never exceed it.
		if kept := strings.TrimPrefix(got, "…"); len(kept) > n {
			t.Fatalf("TailRunes(%d) kept %d bytes, want <= %d", n, len(kept), n)
		}
	}
	if got := TailRunes(s, len(s)); got != s {
		t.Errorf("an uncut string must come back verbatim, got %q", got)
	}
	if got := TailRunes("", 10); got != "" {
		t.Errorf("empty input must stay empty, got %q", got)
	}
}

// TestAgentTypeComparisonTreatsPlaceholdersAsUnknown pins the distinction that
// a bare `!=` gets wrong: "unknown" is a stored VALUE hap writes when herdr
// reported no type, so comparing it against a real type is an absence of
// evidence, not a mismatch. Code that refuses on it refuses the wrong thing.
func TestAgentTypeComparisonTreatsPlaceholdersAsUnknown(t *testing.T) {
	cases := []struct {
		a, b            string
		same, different bool
	}{
		{"claude", "claude", true, false},
		{"claude", "CLAUDE", true, false}, // case-insensitive
		{" claude ", "claude", true, false},
		{"claude", "codex", false, true},
		// Placeholders yield NEITHER conclusion.
		{"unknown", "claude", false, false},
		{"claude", "unknown", false, false},
		{"undefined", "claude", false, false},
		{"", "claude", false, false},
		{"unknown", "unknown", false, false},
	}
	for _, tc := range cases {
		if got := SameAgentType(tc.a, tc.b); got != tc.same {
			t.Errorf("SameAgentType(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.same)
		}
		if got := DifferentAgentType(tc.a, tc.b); got != tc.different {
			t.Errorf("DifferentAgentType(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.different)
		}
	}
}
