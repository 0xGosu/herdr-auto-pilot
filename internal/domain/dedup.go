package domain

import (
	"regexp"
	"strings"
)

// Herdr re-delivers an attention event for the same agent after a delay: the
// agent flips done->idle when the operator reads the pane, and the event fires
// again carrying the same screen. Re-raising an escalation for it would ask the
// operator the same question twice. This file holds the pure half of the
// duplicate-ask check: what counts as "the same screen again".

// Volatile agent-TUI chrome (FR-003). These lines repaint on a timer and carry
// no decision content, so two captures of one standing screen differ only
// here. Each pattern is justified by an observed render:
//
//	✻ Waiting for API response · will retry in 2m 2s · check your network
//	✽ Thinking… (12s · ↑ 1.2k tokens · esc to interrupt)
//
// The elision is deliberately LINE-scoped rather than token-scoped. Masking
// individual tokens is what makes MaskVolatile unusable here (see
// NormalizeForDedup): a token masker that eats "2s" also eats the "5s" in
// `Bash(sleep 5s)`. A whole chrome LINE can be dropped safely because nothing
// in it is ever the question being asked.
var (
	// dedupSpinnerLineRE matches a spinner/status header by its leading glyph.
	// Only the star frames the agent TUIs use EXCLUSIVELY for the animated
	// working line are listed — they cycle (✽ ✻ ✶ ✳ ✢) between captures of one
	// motionless screen. Deliberately NOT included: ● (Claude's SETTLED
	// assistant-message bullet — a stable content line, e.g. "● All tests
	// pass."), and · / * (which also begin separators and markdown bullets).
	// Eliding those would risk collapsing two genuinely different escalations,
	// silently dropping a real operator ask.
	dedupSpinnerLineRE = regexp.MustCompile(`^[✽✻✶✳✢]\s`)
	// dedupChromeMarkers are substrings that mark a line as agent chrome
	// carrying a live counter (elapsed time, countdown, token usage). These
	// catch the volatile status/retry lines regardless of their current glyph.
	dedupChromeMarkers = []string{"esc to interrupt", "will retry in"}
	// dedupWhitespaceRE collapses whitespace runs so a repaint that shifts
	// padding or re-wraps a line still reads as the same screen.
	dedupWhitespaceRE = regexp.MustCompile(`\s+`)
)

// dedupChromePlaceholder replaces an elided chrome line. A placeholder rather
// than a deletion so a screen that GAINS or LOSES a chrome line is still
// recognizably different.
const dedupChromePlaceholder = "<chrome>"

// NormalizeForDedup canonicalizes captured pane content for the duplicate
// check: elide volatile agent-TUI chrome lines, then collapse whitespace runs.
//
// It deliberately does NOT use MaskVolatile, and that is the whole design.
// MaskVolatile is the LEARNING primitive: it replaces <path>/<num>/<hash> so
// paraphrases of one situation collapse onto a single signature. Those are
// exactly the tokens that distinguish two consecutive but DIFFERENT questions
// from one agent — verified: "Bash(rm -rf /tmp/foo)" and "Bash(rm -rf
// /tmp/bar)" both mask to "Bash(rm -rf <path>)". Because herdr's pane read is
// a consuming delta (--source recent), a second capture may carry no
// scrollback to tell them apart, so masking here would silently swallow a real
// question and strand the agent — the "never a silent drop" rule.
//
// Dedup asks "is this the same screen again?", not "is this the same KIND of
// screen?". So paths, commands and numbers are left untouched and only chrome
// that provably repaints on its own is elided. An unrecognized jitter source
// therefore fails SAFE: the captures compare unequal and the event is
// processed, exactly as it is today.
func NormalizeForDedup(s string) string {
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		if isDedupChromeLine(strings.TrimSpace(ln)) {
			lines[i] = dedupChromePlaceholder
		}
	}
	return strings.TrimSpace(dedupWhitespaceRE.ReplaceAllString(strings.Join(lines, "\n"), " "))
}

// isDedupChromeLine reports whether a trimmed line is volatile agent chrome.
func isDedupChromeLine(trimmed string) bool {
	if trimmed == "" {
		return false
	}
	if dedupSpinnerLineRE.MatchString(trimmed) {
		return true
	}
	for _, m := range dedupChromeMarkers {
		if strings.Contains(trimmed, m) {
			return true
		}
	}
	return false
}

// PendingEscalation is one escalation still awaiting the operator, narrowed to
// the fields the duplicate-ask check may look at. Narrowed on purpose: it omits
// the agent status (and AuditRecord.Trigger, which embeds it as "agent-status:
// idle") so a caller cannot key on the field that legitimately CHANGES between
// the duplicates.
type PendingEscalation struct {
	SituationType SituationType
	PaneExcerpt   string
}

// DuplicatesPendingEscalation reports whether a fresh capture repeats an
// escalation already awaiting the operator — so re-raising it would just ask
// the same question twice. `pending` must be pre-scoped to one agent + agent
// type (the store does this).
//
// The key is agent + agent type + the normalized pane content. Neither the
// agent status nor the situation type participates:
//   - agent_status is exactly what changes between the duplicates (done->idle
//     when the operator reads the pane); AuditRecord.Trigger embeds it, which
//     is why PendingEscalation omits it.
//   - situation_type is DERIVED from the status (Classifier.Classify gates the
//     approval/choice rules on herdr reporting "blocked"), so one standing
//     screen re-fired as idle reclassifies. Keying on it would miss the very
//     re-fire this exists to catch.
//
// The one exception is empty content: with no content to compare, the type is
// the only identity left, so an empty-excerpt escalation must also match on
// type. This keeps the herdr-unreachable path (which has no pane to read, and
// legitimately keys on "") from matching an empty-excerpt escalation of a
// different kind.
func DuplicatesPendingEscalation(sitType SituationType, excerpt string, pending []PendingEscalation) bool {
	key := NormalizeForDedup(excerpt)
	for _, p := range pending {
		if NormalizeForDedup(p.PaneExcerpt) != key {
			continue
		}
		// No content to discriminate on: fall back to the situation type.
		if key == "" && p.SituationType != sitType {
			continue
		}
		return true
	}
	return false
}
