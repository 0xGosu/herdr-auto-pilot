package domain

import (
	"regexp"
	"strings"
)

// Codex surfaces a blocking interrupt screen when a run is manually
// interrupted (e.g. Escape mid-turn): "Conversation interrupted - tell the
// model what to do differently." codexInterruptedRE is anchored on that
// distinctive phrasing, with a small bounded gap for the "-" separator (and
// minor spacing drift), so ordinary narration that merely contains the word
// "interrupted" (a printed log line, a description of some other
// interruption) does not become a live error.
var (
	codexInterruptedRE = regexp.MustCompile(`(?i)conversation interrupted\b.{0,20}tell the model what to do differently`)

	// Codex parks this model-switch reminder at the bottom of the pane when
	// account credits approach their limit. The header/question/footer form a
	// narrow live-modal envelope; the footer's \z anchor prevents an older copy
	// in scrollback from being treated as a current error.
	codexRateLimitHeaderRE   = regexp.MustCompile(`(?im)^[ \t]*Approaching rate limits[ \t]*$`)
	codexRateLimitQuestionRE = regexp.MustCompile(`(?im)^[ \t]*Switch to[ \t]+.+[ \t]+for lower credit usage\?[ \t]*$`)
	codexRateLimitFooterRE   = regexp.MustCompile(`(?im)^[ \t]*Press enter to confirm or esc to go back[ \t]*\s*\z`)
)

// Stable ErrorSummary label for Codex's built-in error forms — used as the
// error signature (`error:<kind>`) so paraphrased instances dedup to one
// learned signature.
const (
	CodexErrorInterrupted = "interrupted"
	CodexErrorRateLimit   = "rate-limit"
)

// CodexErrorForm reports whether pane content shows one of Codex's blocking
// error/interrupt conditions, and which kind. kind is "" exactly when ok is
// false.
func CodexErrorForm(pane string) (kind string, ok bool) {
	switch {
	case CodexRateLimitForm(pane):
		return CodexErrorRateLimit, true
	case codexInterruptedRE.MatchString(pane):
		return CodexErrorInterrupted, true
	}
	return "", false
}

// CodexRateLimitForm reports whether pane ends in Codex's live
// "Approaching rate limits" model-switch modal. Callers must additionally
// gate it on agent_type == "codex".
func CodexRateLimitForm(pane string) bool {
	region := ExtractCodexRateLimitForm(pane)
	if region == "" || !codexRateLimitQuestionRE.MatchString(region) {
		return false
	}
	opts := ParseNumberedOptions(region)
	if len(opts) != 3 || opts[0].Number != "1" || opts[1].Number != "2" || opts[2].Number != "3" {
		return false
	}
	labels := []string{
		strings.ToLower(strings.TrimSpace(opts[0].Label)),
		strings.ToLower(strings.TrimSpace(opts[1].Label)),
		strings.ToLower(strings.TrimSpace(opts[2].Label)),
	}
	return strings.HasPrefix(labels[0], "switch to ") &&
		labels[1] == "keep current model" &&
		strings.HasPrefix(labels[2], "keep current model (never show again)")
}

// ExtractCodexRateLimitForm returns only the final live modal, excluding
// preceding conversation and any numbered lists it may contain.
func ExtractCodexRateLimitForm(pane string) string {
	headers := codexRateLimitHeaderRE.FindAllStringIndex(pane, -1)
	footer := codexRateLimitFooterRE.FindStringIndex(pane)
	if len(headers) == 0 || footer == nil {
		return ""
	}
	header := headers[len(headers)-1]
	if footer[0] <= header[0] {
		return ""
	}
	return strings.TrimSpace(pane[header[0]:footer[1]])
}
