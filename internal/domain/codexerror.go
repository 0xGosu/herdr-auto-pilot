package domain

import "regexp"

// Codex surfaces a blocking interrupt screen when a run is manually
// interrupted (e.g. Escape mid-turn): "Conversation interrupted - tell the
// model what to do differently." codexInterruptedRE is anchored on that
// distinctive phrasing, with a small bounded gap for the "-" separator (and
// minor spacing drift), so ordinary narration that merely contains the word
// "interrupted" (a printed log line, a description of some other
// interruption) does not become a live error.
var codexInterruptedRE = regexp.MustCompile(`(?i)conversation interrupted\b.{0,20}tell the model what to do differently`)

// Stable ErrorSummary label for Codex's built-in error forms — used as the
// error signature (`error:<kind>`) so paraphrased instances dedup to one
// learned signature.
const (
	CodexErrorInterrupted = "interrupted"
)

// CodexErrorForm reports whether pane content shows one of Codex's blocking
// error/interrupt conditions, and which kind. kind is "" exactly when ok is
// false.
func CodexErrorForm(pane string) (kind string, ok bool) {
	switch {
	case codexInterruptedRE.MatchString(pane):
		return CodexErrorInterrupted, true
	}
	return "", false
}
