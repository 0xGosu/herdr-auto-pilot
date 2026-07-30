package domain

import (
	"regexp"
	"strings"
)

// Claude Code's TUI chrome, as it lands in a pane capture. None of it is ever
// the thing being decided, but all of it is byte-identical across unrelated
// panes, so leaving it in the salient does two kinds of damage at once:
//
//   - it INFLATES similarity between different screens. Two idle panes whose
//     only real difference is one sentence share every banner, rule, status and
//     mode line, and an embedding of the pair lands well above
//     similarity_threshold — the "matched a near-empty saved rule at cosine
//     0.91" report this exists to fix.
//   - it EATS the salient window. salientContent keeps only the trailing
//     pane_salient_chars characters, and the footer block alone can be a few
//     hundred of them, pushing the agent's actual output out of the signature.
//
// The design mirrors StripCodexComposer (codex.go): a pure, line-oriented
// cleaner, gated by the caller on agent type, that only ever DELETES lines it
// can positively identify. An unrecognized line is kept, which fails safe —
// two different screens stay different and the situation escalates, exactly as
// it does today.
var (
	// claudeRuleLineRE matches the full-width horizontal rules Claude draws
	// around the composer. A rule carries no content in any situation.
	claudeRuleLineRE = regexp.MustCompile(`^[─━]{8,}$`)
	// claudeModeLineRE matches the permission/vim mode footer:
	// "-- INSERT -- ⏵⏵ accept edits on (shift+tab to cycle) · ← for agents",
	// and its siblings ("⏵⏵ bypass permissions on", "⏸ plan mode on").
	//
	// Every alternative is LINE-ANCHORED, and that matters: an unanchored
	// substring test would delete any footer-window line merely containing the
	// marker, and the footer window is the whole capture on a short pane. An
	// agent quoting Claude's own UI — "(shift+tab to cycle) switches mode" —
	// would lose the line, which is the one failure mode this file forbids.
	// "(shift+tab to cycle)" is therefore not an alternative at all: it never
	// appears without a mode glyph or the vim indicator ahead of it.
	claudeModeLineRE = regexp.MustCompile(`^(?:--\s*INSERT\s*--|[⏵⏸])`)
	// claudeStatusBarTailRE matches the wide padding run before herdr's
	// trailing status token ("…019083a8f31f        focus"). Terminal-width
	// padding is what separates the status bar from a shell pipeline or a
	// pipe-separated sentence, which never carry it.
	claudeStatusBarTailRE = regexp.MustCompile(`\s{2,}\S+$`)
	// claudeComposerLineRE matches the input line. It is stripped ONLY as the
	// last non-empty line (see StripClaudeChrome): "❯" is also the caret an
	// option list draws in front of the highlighted choice, and that caret is
	// never the final line of a form.
	claudeComposerLineRE = regexp.MustCompile(`^❯`)
)

// claudeBannerGlyphs are the glyphs Claude Code's startup logo is drawn from.
var claudeBannerGlyphs = map[rune]bool{
	'▐': true, '▛': true, '▜': true, '▌': true, '▝': true, '▘': true, '█': true,
}

// claudeBannerCornerGlyphs are the logo's QUADRANT and half-block glyphs — the
// subset that "█" is not. Requiring these is what separates the logo from a
// progress bar or bar chart the agent printed: a bar is drawn from solid blocks
// (and shade glyphs), essentially never from the corner pieces the logo's
// silhouette needs.
var claudeBannerCornerGlyphs = map[rune]bool{
	'▐': true, '▛': true, '▜': true, '▌': true, '▝': true, '▘': true,
}

// claudeBannerMarker corroborates the whole banner block. The logo's first row
// always carries the product name, so its ABSENCE from the head of a capture
// means whatever block glyphs are there belong to the agent's own output, not
// to a banner. This is the load-bearing half of the check: a pane capture does
// not guarantee the logo is still on screen — herdr's "recent" read is a
// consuming delta, and a scrolled pane can begin mid-output — so without the
// marker a legitimate "████████ 80% done" as the FIRST captured line would be
// deleted, and two screens differing only in bar length would collapse onto one
// signature.
const claudeBannerMarker = "Claude Code"

// claudeFooterLines bounds how far back from the end of the capture the
// footer-only filters (mode line, herdr status bar) may look. Those two shapes
// are less distinctive than the banner or the rules — a sentence of agent prose
// could carry three pipes — so they are only recognized where the footer
// actually renders: at the bottom of the screen.
const claudeFooterLines = 8

// claudeBannerLines bounds the banner filter to the head of the capture, the
// only place the startup logo renders. The bound alone is NOT sufficient — a
// scrolled or delta capture can begin with the agent's own block-glyph output —
// so it is one of three pieces of evidence, alongside claudeBannerMarker and
// the per-line corner-glyph structure.
const claudeBannerLines = 6

// claudeStatusBarFields is the minimum number of "|" separators for a trailing
// line to read as herdr's status bar
// ("workspace | Fable 5 (7%) | default | <uuid>   focus").
const claudeStatusBarFields = 3

// StripClaudeChrome removes Claude Code's TUI furniture from pane text: the
// startup banner, the horizontal rules, the live spinner/progress line, the
// permission-mode footer, herdr's status bar, and the trailing composer line.
// Everything the agent actually said survives.
//
// Callers MUST gate this on agent_type == "claude". The glyphs it keys off
// ("❯", "⏵⏵", the block-drawing banner run) carry no special meaning for other
// agent types, and "❯" in particular is a plain option caret elsewhere.
//
// Deliberately NOT routed through NormalizeForDedup: that normalizer answers
// "is this the same screen again?" and replaces chrome with a <chrome>
// PLACEHOLDER so a screen that gains or loses a chrome line stays unequal (see
// the design note above NormalizeForDedup). The salient answers "is this the
// same KIND of screen?", so it wants the chrome gone entirely. Only the
// line-level chrome PREDICATE (isDedupChromeLine — spinner frames and the
// "esc to interrupt" / "will retry in" live counters) is shared, because that
// judgement is the same in both places.
func StripClaudeChrome(pane string) string {
	lines := strings.Split(pane, "\n")
	footerFrom := len(lines) - claudeFooterLines
	// The banner is removed as a located BLOCK, not as "any glyph line near the
	// top": only the rows between bannerStart and bannerEnd go, and only when a
	// row was found that is both glyph-shaped and carries the product name. On
	// every other capture nothing here is stripped, so the agent's own
	// block-glyph output is never at risk (see claudeBannerBlock).
	bannerStart, bannerEnd, bannerFound := claudeBannerBlock(lines)
	kept := make([]string, 0, len(lines))
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if bannerFound && i >= bannerStart && i <= bannerEnd {
			continue
		}
		if claudeRuleLineRE.MatchString(trimmed) {
			continue
		}
		// The live status line — "✽ Thinking… (12s · ↑ 1.2k tokens · esc to
		// interrupt)", "✻ API error — will retry in 4s" — repaints on its own
		// between two captures of one motionless screen.
		//
		// Only the LEADING spinner glyph identifies it. Deliberately NOT reused
		// from isDedupChromeLine: that predicate also fires on the substrings
		// "esc to interrupt" / "will retry in" ANYWHERE in a line, which is
		// safe for dedup (it substitutes a <chrome> placeholder) but not here.
		// Deleting a whole line because a sentence mentions the phrase would
		// drop real content and let two different screens collapse onto one
		// signature — the one failure mode this file must not introduce. A
		// spinner-less status line therefore survives, and the residual jitter
		// it contributes is absorbed by SignatureHeldStill's existing fuzzy
		// tolerance, exactly as before.
		if dedupSpinnerLineRE.MatchString(trimmed) {
			continue
		}
		if i >= footerFrom && (claudeModeLineRE.MatchString(trimmed) || claudeStatusBarLine(trimmed)) {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(dropTrailingComposer(kept), "\n")
}

// dropTrailingComposer removes the composer line once it is the last non-empty
// line — i.e. after the rules, status bar and mode line below it are already
// gone. Anchoring on "last" rather than on the glyph is what keeps an option
// list's "❯ 1. Read-only" caret intact: a form always renders something after
// its highlighted option (the remaining options, its footer hints), so the
// caret is never last. Blank lines trailing the composer are dropped with it.
func dropTrailingComposer(lines []string) []string {
	for i := len(lines) - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" {
			continue
		}
		if claudeComposerLineRE.MatchString(trimmed) {
			return lines[:i]
		}
		return lines
	}
	return lines
}

// claudeBannerBlock locates the startup logo as a STRUCTURE and returns the
// inclusive line range it occupies, or ok=false when the capture does not show
// one.
//
// The marker and the glyph shape must hold on the SAME line — the logo's first
// row carries the product name itself ("▐▛███▜▌   Claude Code v2.1.220"). Testing
// them independently over the head is not enough, because ordinary output can
// satisfy each separately: a report headed "Claude Code output:" followed by
// "▌▌▌ 30% indexed" would arm the filter and then delete the progress line,
// which is real content. Requiring one line to be both is what makes the
// evidence a banner rather than a coincidence.
//
// From that anchor the block extends over the contiguous glyph rows beneath it
// (the logo is three rows, the last carrying the cwd), bounded by
// claudeBannerLines. A capture that begins PART-WAY into the logo has no anchor
// row, so nothing is stripped — chrome survives into the salient, which is the
// safe direction: it never deletes real output.
func claudeBannerBlock(lines []string) (start, end int, ok bool) {
	limit := len(lines)
	if limit > claudeBannerLines {
		limit = claudeBannerLines
	}
	for i := 0; i < limit; i++ {
		if !claudeBannerLine(strings.TrimSpace(lines[i])) ||
			!strings.Contains(lines[i], claudeBannerMarker) {
			continue
		}
		end = i
		for j := i + 1; j < limit; j++ {
			if !claudeBannerLine(strings.TrimSpace(lines[j])) {
				break
			}
			end = j
		}
		return i, end, true
	}
	return 0, 0, false
}

// claudeBannerLine reports whether a trimmed line is part of the startup logo.
// It requires the line to BEGIN with a logo glyph and to carry at least two
// CORNER glyphs — the quadrant/half-block pieces the logo's silhouette is built
// from, which "█" is not.
//
// The corner requirement is what keeps a bar out: "████████ 80% done" is drawn
// from solid blocks and has no corner pieces, so it is content even when the
// banner filter is armed. It covers all three banner rows, including the one
// whose only other content is the cwd ("▘▘ ▝▝    /path").
func claudeBannerLine(trimmed string) bool {
	runes := []rune(trimmed)
	if len(runes) == 0 || !claudeBannerGlyphs[runes[0]] {
		return false
	}
	n := 0
	for _, r := range runes {
		if claudeBannerCornerGlyphs[r] {
			n++
		}
	}
	return n >= 2
}

// claudeStatusBarLine reports whether a trimmed line is herdr's bottom status
// bar ("workspace | Fable 5 (7%) | default | <uuid>        focus").
//
// The pipe count alone is NOT sufficient evidence, and relying on it was a real
// bug: a shell pipeline the agent reported running — "● Ran: git log --oneline |
// grep fix | head -3 | cat" — carries three pipes too, and the footer window is
// the whole capture for a short pane, so deleting on pipes alone would drop real
// content and let two different screens collapse onto one signature. Three
// pieces of evidence are therefore required together: at least
// claudeStatusBarFields pipes, no leading "|" (a markdown table row), and the
// terminal-width padding run before the trailing status token, which only a
// status bar has.
func claudeStatusBarLine(trimmed string) bool {
	if trimmed == "" || strings.HasPrefix(trimmed, "|") {
		return false
	}
	if strings.Count(trimmed, "|") < claudeStatusBarFields {
		return false
	}
	return claudeStatusBarTailRE.MatchString(trimmed)
}
