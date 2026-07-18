package domain

import (
	"regexp"
	"strings"
	"unicode/utf8"
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

	// Codex's hard usage-limit stop ("■ You've hit your usage limit. Upgrade
	// to Pro …, purchase more credits or try again at <date>."). Mirrors
	// claudeLimitRE's apostrophe tolerance; the volatile reset date never
	// enters the signature because the kind label below is the ErrorSummary.
	// Matched only against a live extracted banner (never the whole pane), so
	// a stale copy in scrollback cannot re-error a recovered agent.
	codexUsageLimitRE = regexp.MustCompile(`(?i)you['’]?ve hit your usage limit\b`)

	// Codex parks this model-switch reminder at the bottom of the pane when
	// account credits approach their limit. The header/question/footer form a
	// narrow live-modal envelope; the footer's \z anchor prevents an older copy
	// in scrollback from being treated as a current error.
	codexRateLimitHeaderRE   = regexp.MustCompile(`(?im)^[ \t]*Approaching rate limits[ \t]*$`)
	codexRateLimitQuestionRE = regexp.MustCompile(`(?im)^[ \t]*Switch to[ \t]+.+[ \t]+for lower credit usage\?[ \t]*$`)
	codexRateLimitFooterRE   = regexp.MustCompile(`(?im)^[ \t]*Press enter to confirm or esc to go back[ \t]*\s*\z`)

	// Codex prefixes blocking error/notice banners with "■" at line start.
	codexBannerLineRE = regexp.MustCompile(`(?m)^[ \t]*■[ \t]+\S`)
	// Codex's live composer/input-box line ("›", possibly with typed text).
	codexComposerLineRE = regexp.MustCompile(`^[ \t]*›`)
	// The composer status footer: model name and cwd joined by " · /"
	// (e.g. "gpt-5.6-sol high · /tmp", a deleted cwd renders as
	// "/path (deleted)"). Full-line shape, same " · /" separator
	// codexComposerBeforeFooterRE keys on; like that regex this is a text
	// heuristic — a final narration line of exactly this shape right after a
	// scrollback banner would be misread as the footer (accepted risk, see
	// the codexComposerBeforeFooterRE comment).
	codexStatusFooterLineRE = regexp.MustCompile(`^[ \t]*\S[^\n·]*\s·\s+/\S*( \(deleted\))?[ \t]*$`)
)

// Stable ErrorSummary label for Codex's built-in error forms — used as the
// error signature (`error:<kind>`) so paraphrased instances dedup to one
// learned signature.
const (
	CodexErrorInterrupted = "interrupted"
	CodexErrorRateLimit   = "rate-limit"
	CodexErrorUsageLimit  = "usage-limit"
)

// codexBannerSummaryMax bounds the generic-banner ErrorSummary so a very long
// banner cannot bloat the signature salient.
const codexBannerSummaryMax = 120

// CodexErrorForm reports whether pane content shows one of Codex's blocking
// error/interrupt conditions, and which kind. The rate-limit modal and the
// interrupt screen keep their existing pane-wide detection (both verified
// live); every other "■" banner is recognized only when it stands live at the
// end of the pane (ExtractCodexErrorBanner) — a known banner (the hard usage
// limit) yields its stable kind label, and any unknown banner (network drops,
// auth failures, quota variants) falls back to a masked-text label so it
// still classifies as an error while distinct banners keep distinct
// signatures. kind is "" exactly when ok is false.
func CodexErrorForm(pane string) (kind string, ok bool) {
	switch {
	case CodexRateLimitForm(pane):
		return CodexErrorRateLimit, true
	case codexInterruptedRE.MatchString(pane):
		return CodexErrorInterrupted, true
	}
	banner := ExtractCodexErrorBanner(pane)
	if banner == "" {
		return "", false
	}
	if codexUsageLimitRE.MatchString(banner) {
		return CodexErrorUsageLimit, true
	}
	summary := MaskVolatile(banner)
	if len(summary) > codexBannerSummaryMax {
		cut := codexBannerSummaryMax
		for cut > 0 && !utf8.RuneStart(summary[cut]) {
			cut--
		}
		summary = summary[:cut]
	}
	return "banner: " + summary, true
}

// ExtractCodexErrorBanner returns the text of a live "■"-prefixed banner
// standing at the end of the pane: the last line-leading "■" block (the "■"
// line plus contiguous wrapped continuation lines), after which only blank
// lines and the composer UI chrome — at most one "›" composer line (present
// when the pane was not run through StripCodexComposer) followed by at most
// one status-footer line (model · cwd) — may remain. Anything else after the
// block means the banner is scrollback, not a live blocking condition, and
// "" is returned. Callers must additionally gate this on agent_type ==
// "codex".
func ExtractCodexErrorBanner(pane string) string {
	locs := codexBannerLineRE.FindAllStringIndex(pane, -1)
	if len(locs) == 0 {
		return ""
	}
	tail := pane[locs[len(locs)-1][0]:]
	lines := strings.Split(tail, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, "\r")
	}
	// The banner block: leading run of contiguous non-blank lines.
	end := len(lines)
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			end = i
			break
		}
	}
	var rest []string
	for _, line := range lines[end:] {
		if strings.TrimSpace(line) != "" {
			rest = append(rest, line)
		}
	}
	if len(rest) > 0 && codexComposerLineRE.MatchString(rest[0]) {
		rest = rest[1:]
	}
	if len(rest) > 0 && codexStatusFooterLineRE.MatchString(rest[0]) {
		rest = rest[1:]
	}
	if len(rest) != 0 {
		return ""
	}
	banner := strings.TrimSpace(strings.Join(lines[:end], " "))
	return strings.TrimSpace(strings.TrimPrefix(banner, "■"))
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
