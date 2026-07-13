package domain

import "regexp"

// Claude Code surfaces a few blocking conditions that need operator attention
// rather than an auto-answer: a usage-limit stop ("You've hit your limit ·
// resets 1am") and an interrupt prompt ("Interrupted · What should Claude do
// instead?"). These are the error/retry situations for claude — deliberately
// narrow so ordinary error-shaped narration (a printed stack trace, a build
// log, an "exit code 1" line) is NOT classified as a live error.
var (
	// claudeLimitRE tolerates a straight or curly apostrophe and an optional
	// "usage" qualifier ("You've hit your limit" / "you've hit your usage limit").
	claudeLimitRE = regexp.MustCompile(`(?i)you['’]?ve hit your (?:usage )?limit`)
	// claudeInterruptedRE keys on the distinctive interrupt-prompt tail; the
	// bounded gap tolerates the "·" separator (and minor spacing drift) while
	// staying on one line so it can't span unrelated narration.
	claudeInterruptedRE = regexp.MustCompile(`(?i)interrupted\b.{0,12}what should claude do instead`)
	// claudeAPIResponseRetryRE matches Claude Code's live API/network retry
	// status. Keep all separators and phrases in the signal so ordinary prose
	// about waiting for an API or checking a network does not become an error.
	// The countdown may contain any combination of hour, minute, and second
	// components (for example "2m 2s" or "45s"). Horizontal whitespace keeps
	// the match confined to the rendered status line.
	claudeAPIResponseRetryRE = regexp.MustCompile(`(?i)waiting[ \t]+for[ \t]+api[ \t]+response[ \t]*·[ \t]*will[ \t]+retry[ \t]+in[ \t]+(?:\d+[hms][ \t]*)+·[ \t]*check[ \t]+your[ \t]+network\b`)
)

// Stable ErrorSummary labels for Claude's built-in error forms — used as the
// error signature (`error:<kind>`) so paraphrased instances (different reset
// times, preceding narration) dedup to one learned signature.
const (
	ClaudeErrorLimit       = "usage-limit"
	ClaudeErrorInterrupted = "interrupted"
	ClaudeErrorAPIRetry    = "api-response-retry"
)

// ClaudeErrorForm reports whether pane content shows one of Claude Code's
// blocking error/interrupt conditions, and which kind. It is the error-
// classification signal for claude; other agent types get their own rules in
// future. kind is "" exactly when ok is false.
func ClaudeErrorForm(pane string) (kind string, ok bool) {
	switch {
	case claudeLimitRE.MatchString(pane):
		return ClaudeErrorLimit, true
	case claudeInterruptedRE.MatchString(pane):
		return ClaudeErrorInterrupted, true
	case claudeAPIResponseRetryRE.MatchString(pane):
		return ClaudeErrorAPIRetry, true
	}
	return "", false
}
