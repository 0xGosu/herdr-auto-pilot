package domain

import (
	"regexp"
	"strings"
	"unicode/utf8"
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

// Claude-injected note lines — the "※ recap: …" session summary (terminated by
// its "(disable recaps in /config)" line) and the rotating "※ Tip: …" hints.
// Unlike spinner lines, these APPEAR on their own on an otherwise motionless
// screen (the recap renders after the agent settles — verified live
// 2026-07-17, escalations #816/#817), so they are DELETED rather than replaced
// with dedupChromePlaceholder: a placeholder would keep "screen without recap"
// and "same screen with recap" unequal, which is the exact duplicate this
// elision exists to absorb.
//
// The trigger is anchored to the two observed note shapes, NOT the bare ※
// glyph: pasted or quoted content can begin with ※ (it is a common note marker
// in Japanese text), and deleting an unrecognized ※ line could collapse two
// genuinely different screens — a silent drop of a real question. An
// unrecognized ※ line is kept, so it fails safe (the captures compare unequal
// and the event escalates, exactly as before this elision existed).
const (
	dedupNoteGlyph       = "※"
	dedupRecapTerminator = "(disable recaps in /config)"
	// dedupRecapMaxLines bounds the terminator look-ahead: a real recap wraps
	// onto at most a handful of physical lines before its terminator.
	dedupRecapMaxLines = 8
)

// dedupNoteLine classifies a trimmed line as a note marker: 0 = not a note,
// 1 = a single-line note ("※ Tip: …"), 2 = a recap marker ("※ recap: …",
// whose wrapped continuation lines may be deleted through the terminator).
func dedupNoteLine(trimmed string) int {
	rest, ok := strings.CutPrefix(trimmed, dedupNoteGlyph)
	if !ok {
		return 0
	}
	rest = strings.ToLower(strings.TrimSpace(rest))
	switch {
	case strings.HasPrefix(rest, "recap:"):
		return 2
	case strings.HasPrefix(rest, "tip:"):
		return 1
	}
	return 0
}

// NormalizeForDedup canonicalizes captured pane content for the duplicate
// check: delete Claude's ※-led recap/tip note lines (they appear on their own
// on a settled screen — see dedupNoteLine), elide volatile agent-TUI chrome
// lines, then collapse whitespace runs. A recap's wrapped continuation lines
// are deleted only when its "(disable recaps in /config)" terminator is found
// within the same non-blank run — without the terminator only the marker line
// is deleted, so plain text adjacent to a recap marker can never be mistaken
// for its continuation.
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
	kept := make([]string, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		switch dedupNoteLine(trimmed) {
		case 1: // single-line note: delete just this line
			continue
		case 2: // recap: delete through the terminator if it is in reach
			if end, ok := recapBlockEnd(lines, i); ok {
				i = end
			}
			continue
		}
		if isDedupChromeLine(trimmed) {
			kept = append(kept, dedupChromePlaceholder)
			continue
		}
		kept = append(kept, lines[i])
	}
	return strings.TrimSpace(dedupWhitespaceRE.ReplaceAllString(strings.Join(kept, "\n"), " "))
}

// recapBlockEnd looks ahead from a recap marker line for the recap's
// terminator within the same non-blank run (a wrapped recap is consecutive
// non-blank lines; Claude separates blocks with blank lines), bounded by
// dedupRecapMaxLines. It returns the terminator's index when found. When the
// terminator is out of reach — cut off by the truncation window, or the ※ line
// was not really a recap — it reports false and the caller deletes only the
// marker line, so real content adjacent to the marker is never swallowed.
func recapBlockEnd(lines []string, start int) (int, bool) {
	for j := start + 1; j < len(lines) && j <= start+dedupRecapMaxLines; j++ {
		trimmed := strings.TrimSpace(lines[j])
		if trimmed == "" {
			return 0, false
		}
		if strings.Contains(trimmed, dedupRecapTerminator) {
			return j, true
		}
	}
	return 0, false
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
//
// Beyond the exact compare, a tail-window-shift tolerant compare absorbs the
// second way one standing screen re-captures differently: excerpts are the
// TRAILING snapshotCap runes of the pane, so anything the agent TUI appends
// drags older text off the window's head. The tail match then succeeds when
// the appended material is itself deleted by normalization (the ※ note
// blocks) — the two captures differ only in how much scrollback the head
// retains, and, first line dropped (it may be cut mid-line by the
// truncation), one normalized capture is the exact tail of the other: the
// same screen with less head context. Appended material that SURVIVES
// normalization keeps the tails unequal and the event escalates — the safe
// direction. A genuinely NEW
// question can never satisfy this: terminal content only appends at the
// bottom, so new output lands between the old content and the prompt and
// breaks the tail match.
//
// snapshotCap is the tail-truncation cap (in runes) the CALLER applied to both
// the fresh excerpt and the stored pending excerpts. The suffix compare only
// runs when both captures fill at least HALF of it. A capture that large is a
// tail window of a longer transcript — herdr's pane read is itself a
// tail-anchored window, so a capture can be head-shifted, its first line cut
// mid-line, WITHOUT hitting the daemon's rune cap (verified: the #816/#817
// excerpts measured 3930/3901 runes against the 4000 cap; NO observable
// provenance says whether a capture's head was cut, so size is the available
// proxy). A small capture, by contrast, is a complete pane whose first line
// is real, possibly the only discriminating content: two approvals for
// different commands share every menu line below the first — dropping it
// would collapse them, a silent drop of a real question. A cap of 0 or less
// disables the suffix compare entirely.
//
// Because a complete pane can also exceed half the cap, size alone is not
// trusted with the first line: firstLineExplained additionally requires the
// dropped line to be a cut fragment of content the OTHER capture retains,
// which is what a genuinely head-shifted window looks like and what two
// first-line-discriminated questions never do. See suffixDuplicate for the
// degenerate-length guard.
func DuplicatesPendingEscalation(sitType SituationType, excerpt string, snapshotCap int, pending []PendingEscalation) bool {
	key := NormalizeForDedup(excerpt)
	freshWindowed := snapshotCap > 0 && utf8.RuneCountInString(excerpt)*2 >= snapshotCap
	suffixKey := NormalizeForDedup(dropFirstLine(excerpt))
	for _, p := range pending {
		if NormalizeForDedup(p.PaneExcerpt) == key {
			// No content to discriminate on: fall back to the situation type.
			if key == "" && p.SituationType != sitType {
				continue
			}
			return true
		}
		if freshWindowed && utf8.RuneCountInString(p.PaneExcerpt)*2 >= snapshotCap &&
			firstLineExplained(excerpt, p.PaneExcerpt) &&
			suffixDuplicate(suffixKey, NormalizeForDedup(dropFirstLine(p.PaneExcerpt))) {
			return true
		}
	}
	return false
}

// firstLineExplained reports whether either capture's first line — the line
// dropFirstLine discards before the suffix compare — is explainable as a cut
// fragment of content the OTHER capture retains. Two tail windows onto one
// screen always satisfy this: the more-shifted window's first line is a
// fragment of a line the less-shifted window still holds (even two different
// mid-line cuts of the same line satisfy it, the shorter fragment being a
// substring of the longer). Two different questions whose only difference IS
// their first line — a complete pane's command line above an identical menu —
// satisfy neither direction, so the suffix compare never sees them. A
// mutated-in-place first line (a chrome counter that ticked at the window
// head) fails both directions and escalates: fail open, exactly the pre-fix
// behavior.
func firstLineExplained(a, b string) bool {
	return firstLineIn(a, b) || firstLineIn(b, a)
}

// firstLineIn reports whether src's trimmed first line appears in other. A
// blank first line is trivially explained — dropping it discards nothing.
func firstLineIn(src, other string) bool {
	first, _, _ := strings.Cut(src, "\n")
	first = strings.TrimSpace(first)
	return first == "" || strings.Contains(other, first)
}

// dropFirstLine removes the first line of a tail-windowed capture: both the
// daemon's rune truncation and herdr's own pane read cut at the tail, so the
// window's first line is often a partial line (or a chrome line whose leading
// glyph was cut off, which would then normalize differently in the two
// captures). Callers only use this on captures large enough to be tail
// windows — a small capture's first line is real content. A capture with no
// newline is entirely that unreliable first line, so it yields "" —
// single-line excerpts are served by the exact compare alone.
func dropFirstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return ""
}

// suffixDuplicate reports whether one normalized, first-line-dropped capture
// is the exact tail of the other — the same screen with a head-shifted
// truncation window.
//
// The length ratio guards the degenerate match: the last few hundred
// normalized runes of any capture of one pane are near-constant chrome
// (separator rules, the prompt box, the status bar), so a very short capture —
// a cleared pane, a just-started agent — would tail-match ANY later screen of
// that pane and silently drop a real question. Requiring the shorter capture
// to hold at least two thirds of the longer's content means a match must agree
// on the question region too, not just the trailing chrome. Two thirds leaves
// room for what legitimately shrinks a re-capture of a full window: the
// deleted note block plus head-line jitter is hundreds of runes against the
// 4000-rune snapshot cap. The ratio is measured in runes, matching the unit
// the snapshot cap and this reasoning are stated in (TUI glyphs are 3 bytes
// each, so byte lengths would over-weight glyph-dense regions).
func suffixDuplicate(a, b string) bool {
	shorter, longer := a, b
	// Byte len is fine for ORDERING (a suffix is never byte-longer than its
	// container); only the ratio below needs runes.
	if len(shorter) > len(longer) {
		shorter, longer = longer, shorter
	}
	if shorter == "" ||
		utf8.RuneCountInString(shorter)*3 < utf8.RuneCountInString(longer)*2 {
		return false
	}
	return strings.HasSuffix(longer, shorter)
}
