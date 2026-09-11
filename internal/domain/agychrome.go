package domain

import (
	"regexp"
	"strings"
)

// agy's TUI chrome, as it lands in a pane capture — the analogue of
// StripClaudeChrome (read its header for why chrome must leave a pane-tail
// salient: it is byte-identical across unrelated panes, so it inflates
// similarity AND spends the salient window). The same rules bind this cleaner:
// it is gated by the caller on agent type agy, and it only ever deletes lines
// it can positively identify — an unrecognized line is kept.
//
// What an agy capture carries (docs/designer/agy-support.md §4):
//
//	root ➜ /tmp/x $ agy --model gemini-3.6-flash-low        launching shell line
//	      ▄▀▀▄        Antigravity CLI 1.2.1                  banner: logo + version,
//	     ▀▀▀▀▀▀       you@example.com (Google AI Pro)        account, model (effort)
//	    ▀▀▀▀▀▀▀▀      Gemini 3.6 Flash (Low)                 and cwd
//	   ▄▀▀    ▀▀▄     /tmp/x
//	  ▄▀▀      ▀▀▄
//	────────────────                                         turn separator
//	● Bash(ls) (ctrl+o to expand)                            tool call (suffix is chrome)
//	⣟  Working...                                            spinner
//	                              1 artifact · /artifact to review
//	────────────────   >   ────────────────                  composer sandwich
//	? for shortcuts                 plan · Gemini 3.6 Flash · low    status bar
//
// The model line and the status bar matter most: they differ per session
// (model, effort, mode), so left in they would split one situation into a
// signature per model.
var (
	// agyLogoRowRE matches one row of the startup logo: the art column built
	// from ▄/▀ (with spaces INSIDE the art — "▄▀▀    ▀▀▄"), optionally followed
	// by the text column after a gap of three or more spaces.
	agyLogoRowRE = regexp.MustCompile(`^\s*[▄▀][▄▀ ]*[▄▀](?:\s{3,}\S.*)?$`)
	// agyLogoOnlyRE is a logo row with no text beside it — how first-run
	// screens draw it.
	agyLogoOnlyRE = regexp.MustCompile(`^\s*[▄▀][▄▀ ]*[▄▀]\s*$`)
	// agyBannerMarkerRE is the banner's anchor: the product name and version,
	// on the logo's first row.
	agyBannerMarkerRE = regexp.MustCompile(`Antigravity CLI \d+\.\d+`)
	// agyLaunchLineRE matches the shell line that launched agy (agy renders
	// inline, so it stays on screen above the banner).
	agyLaunchLineRE = regexp.MustCompile(`[$#%➜❯]\s+agy(?:\s|$)`)
	// agySpinnerLineRE matches the live working line: a braille spinner frame
	// then a word ("⣟  Working...", "⢿  Generating...") — the shape herdr's
	// own agy spinner rule keys on.
	agySpinnerLineRE = regexp.MustCompile(`^[\x{2800}-\x{28FF}]+\s+\S`)
	// agyArtifactMarkerRE matches the right-aligned "N artifact · /artifact to
	// review" marker agy draws above the composer.
	agyArtifactMarkerRE = regexp.MustCompile(`^\s{8,}\d+ artifacts? · /artifact to review\s*$`)
)

// agyExpandSuffix is appended to every collapsed tool call.
const agyExpandSuffix = " (ctrl+o to expand)"

// agyBannerLines bounds the banner search to the head of the capture — the
// only place the logo renders. The launching shell line may sit above it.
const agyBannerLines = 8

// agyLogoRows is the logo's height.
const agyLogoRows = 5

// agyLogoMinGlyphs keeps a logo-only row from matching a short run of block
// glyphs an agent printed: the smallest logo row ("▄▀▀▄") has four.
const agyLogoMinGlyphs = 4

// StripAgyChrome removes agy's TUI furniture from pane text: the launching
// shell line, the startup banner, turn-separator rules, the spinner line, the
// artifact marker, the tool-call "(ctrl+o to expand)" suffix, and the trailing
// composer and status bar. Everything agy and the operator actually said
// survives. Callers MUST gate this on agent type agy.
func StripAgyChrome(pane string) string {
	lines := strings.Split(pane, "\n")
	drop := make([]bool, len(lines))
	markAgyBanner(lines, drop)
	kept := make([]string, 0, len(lines))
	for i, line := range lines {
		if drop[i] {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if agyRuleLineRE.MatchString(trimmed) || agySpinnerLineRE.MatchString(trimmed) ||
			agyArtifactMarkerRE.MatchString(line) {
			continue
		}
		kept = append(kept, strings.Replace(line, agyExpandSuffix, "", 1))
	}
	out := strings.Join(dropAgyComposer(kept), "\n")
	if strings.TrimSpace(out) == "" {
		return agyChromeOnly
	}
	return out
}

// markAgyBanner marks the banner rows for deletion. The banner is removed as a
// located BLOCK, armed only by the product-name row: that row must be a logo
// row AND carry "Antigravity CLI <version>", and only the logo rows directly
// beneath it (at most the logo's height) go with it — so the account, model
// and cwd text beside them goes too, and nothing else. The shell line that
// launched agy is dropped only when it sits directly above that anchor.
//
// First-run screens draw the logo with no text at all, so a head row made
// ENTIRELY of logo glyphs (at least agyLogoMinGlyphs of them) is also dropped;
// a progress bar is drawn from █, never ▄/▀, so it is not at risk.
func markAgyBanner(lines []string, drop []bool) {
	limit := min(len(lines), agyBannerLines)
	if markAgyBannerFragment(lines, drop, limit) {
		return
	}
	for i := 0; i < limit; i++ {
		if agyLogoOnlyRE.MatchString(lines[i]) &&
			strings.Count(lines[i], "▄")+strings.Count(lines[i], "▀") >= agyLogoMinGlyphs {
			drop[i] = true
			continue
		}
		if !agyLogoRowRE.MatchString(lines[i]) || !agyBannerMarkerRE.MatchString(lines[i]) {
			continue
		}
		drop[i] = true
		for j := i + 1; j < len(lines) && j < i+agyLogoRows; j++ {
			if !agyLogoRowRE.MatchString(lines[j]) {
				break
			}
			drop[j] = true
		}
		for p := i - 1; p >= 0; p-- {
			if strings.TrimSpace(lines[p]) == "" {
				continue
			}
			if agyLaunchLineRE.MatchString(lines[p]) {
				drop[p] = true
			}
			break
		}
		return
	}
}

// markAgyBannerFragment handles a capture that begins PART-WAY into the logo —
// the common shape of a visible read once agy's transcript has scrolled the
// product-name row off the top. The anchor row is gone, so the evidence is
// structural instead: the capture must OPEN (blank lines aside) with at least
// two consecutive logo rows, each either art-only or art plus a text column
// set off by three or more spaces. Agent output does not open a screen with
// two stacked rows of ▄/▀ art. ok reports that a fragment was found (and
// marked), in which case no anchored banner can follow it.
func markAgyBannerFragment(lines []string, drop []bool, limit int) bool {
	start := 0
	for start < limit && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	end := start
	for end < limit && end < start+agyLogoRows && agyLogoRowRE.MatchString(lines[end]) &&
		!agyBannerMarkerRE.MatchString(lines[end]) {
		end++
	}
	if end-start < 2 {
		return false
	}
	for i := start; i < end; i++ {
		drop[i] = true
	}
	return true
}

// agyChromeOnly stands in for a capture that was NOTHING but agy's chrome — a
// fresh session, or one whose screen shows only the banner and the composer.
// Without it that salient would be empty and the situation over-masked, which
// escalates before an idle agent's task source is ever consulted. Every such
// screen really is the same situation, so one fixed salient is the honest
// identity for it.
const agyChromeOnly = "[agy: nothing on screen but its own chrome - composer at rest]"

// dropAgyComposer removes the status bar and then the composer line once each
// is the last non-empty line — the rules around the composer are already gone.
// Anchoring on "last" is what keeps a menu caret ("> 1. Yes, run command")
// intact: a form always renders its remaining options and key hints below it.
func dropAgyComposer(lines []string) []string {
	lines = trimTrailingBlank(lines)
	if n := len(lines); n > 0 && agyStatusBarLine(lines[n-1]) {
		lines = trimTrailingBlank(lines[:n-1])
	}
	if n := len(lines); n > 0 && strings.HasPrefix(strings.TrimSpace(lines[n-1]), ">") {
		lines = trimTrailingBlank(lines[:n-1])
	}
	return lines
}
