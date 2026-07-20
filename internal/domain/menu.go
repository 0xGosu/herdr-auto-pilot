package domain

import (
	"regexp"
	"sort"
	"strings"
)

// numberedOptionRE matches a numbered menu line as agents render it:
// an optional selection caret, then "N." / "N)" / "[N]", then the label.
// The number is captured so the displayed digit is delivered verbatim (a
// menu that starts at 0 or skips a number is honored, not re-indexed).
var numberedOptionRE = regexp.MustCompile(`(?m)^[ \t]*(?:[❯›>][ \t]*)?(?:(\d+)[.)]|\[(\d+)\])[ \t]+(\S.*?)[ \t]*$`)

// checkboxLabelRE matches the leading checkbox marker that a Claude
// multi-SELECT question renders in front of each toggleable option (e.g.
// "1. [ ] Auto-sends" parses to label "[ ] Auto-sends"). The capture group is
// the box contents: a space (unchecked) or a check mark (checked).
//
// Claude Code renders a CHECKED box as `[✔]` (verified live 2026-07-20,
// v2.1.215) — matching only `[x]` made every checked option invisible to
// OptionCheckStates, so the "this tab already carries a selection" gate saw an
// operator's selection as an empty tab and the toggle verification could not
// see its own keystroke land. `[x]`/`[X]`/`[✓]` stay accepted: cheap, and the
// glyph is a rendering detail that has already changed once.
var checkboxLabelRE = regexp.MustCompile(`^\[([ xX✔✓])\]`)

// NumberedOption pairs a menu option's displayed number with its label.
type NumberedOption struct {
	Number string
	Label  string
}

// MultiSelectTab reports whether a captured MCQ frame shows a multi-SELECT
// question: one whose options carry per-option `[ ]`/`[x]` checkboxes (toggle
// several with digit keys, then advance). A single-select question renders
// plain numbered options with no checkbox and returns false.
func MultiSelectTab(frame string) bool {
	for _, o := range ParseNumberedOptions(frame) {
		if checkboxLabelRE.MatchString(o.Label) {
			return true
		}
	}
	return false
}

// OptionCheckStates returns, for a multi-select frame, each option digit's
// current checkbox state (true = checked). Options without a checkbox marker
// are omitted, so an all-unchecked multi-select tab yields every digit mapped
// to false. Delivery uses this to verify the toggle baseline before typing.
func OptionCheckStates(frame string) map[string]bool {
	states := make(map[string]bool)
	for _, o := range ParseNumberedOptions(frame) {
		if m := checkboxLabelRE.FindStringSubmatch(o.Label); m != nil {
			states[o.Number] = m[1] != " " // any mark means checked
		}
	}
	return states
}

// CheckedOutside lists, in option order, the digits a multi-select frame shows
// as CHECKED that are not in chosen. It is the shared "checked ⊆ chosen" rule
// for answering a checkbox tab: the boxes an answer means to set are safe to
// find already set (its own earlier, unverified attempt put them there, and
// re-pressing one would CLEAR it), while any other checked box belongs to
// someone else and is never hap's to clear.
//
// Pass a nil/empty chosen to demand an all-unchecked frame — the capture-time
// baseline, where no answer has been decided yet.
func CheckedOutside(frame string, chosen []string) []string {
	want := make(map[string]bool, len(chosen))
	for _, digit := range chosen {
		want[digit] = true
	}
	var foreign []string
	for digit, checked := range OptionCheckStates(frame) {
		if checked && !want[digit] {
			foreign = append(foreign, digit)
		}
	}
	sort.Strings(foreign)
	return foreign
}

// ClearCheckboxMarks rewrites a form's selection state back to untouched, so
// two renders of the same form compare equal regardless of what a partial
// delivery toggled. That means the option boxes AND the tab header's answered
// marks: ticking a checkbox flips its tab from ☐ to ☒ while the form still
// stands (verified live 2026-07-20), so normalizing the boxes alone would
// still compare unequal and the comparison would reject the very
// partially-delivered form it exists to accept.
//
// Only those marks change — option text, the ✔ Submit entry, and every other
// line are untouched — and CheckedOutside is what governs which boxes may be
// set, so nothing that decides safety is hidden by the comparison.
func ClearCheckboxMarks(content string) string {
	content = checkedOptionRE.ReplaceAllString(content, "${1}[ ]")
	return mcqTabHeaderRE.ReplaceAllStringFunc(content, func(header string) string {
		return strings.ReplaceAll(header, "☒", "☐")
	})
}

// checkedOptionRE matches a numbered option line's CHECKED box, capturing
// everything up to it so the replacement keeps the caret, number and spacing.
var checkedOptionRE = regexp.MustCompile(`(?m)^([ \t]*(?:[❯›>][ \t]*)?(?:\d+[.)]|\[\d+\])[ \t]+)\[[xX✔✓]\]`)

// ParseNumberedOptions extracts the numbered options from pane content in
// display order (e.g. "❯ 1. Yes\n  2. No" → [{"1","Yes"},{"2","No"}]).
func ParseNumberedOptions(content string) []NumberedOption {
	var opts []NumberedOption
	for _, m := range numberedOptionRE.FindAllStringSubmatch(content, -1) {
		num := m[1]
		if num == "" {
			num = m[2]
		}
		label := strings.TrimSpace(m[3])
		if num != "" && label != "" {
			opts = append(opts, NumberedOption{Number: num, Label: label})
		}
	}
	return opts
}

// OptionLabels returns just the labels of ParseNumberedOptions, for the
// classifier's option set (order-preserving).
func OptionLabels(content string) []string {
	opts := ParseNumberedOptions(content)
	labels := make([]string, 0, len(opts))
	for _, o := range opts {
		labels = append(labels, o.Label)
	}
	return labels
}

// MenuKeystroke maps a chosen response to the digit a numbered menu expects.
//
// Agents like Claude Code render approvals/choices as numbered menus
// ("1. Yes / 2. No") that only accept the option's number — typing the label
// text ("Yes") is ignored, which looked to operators like "nothing happened"
// on confirm. When content presents a numbered menu and chosen matches an
// option — by label (case-insensitive, trimmed) or by an already-numeric
// selection — MenuKeystroke returns that option's digit and true.
//
// It returns (chosen, false) when there is no numbered menu, or chosen
// matches no option: free-text prompts (a typed reply, an error-retry
// command) must be delivered literally, so callers send chosen unchanged.
func MenuKeystroke(content, chosen string) (string, bool) {
	return MenuKeystrokeFrom(ParseNumberedOptions(content), chosen)
}

// MenuKeystrokeFrom is MenuKeystroke over an already-parsed option set, for
// callers whose options carry normalized labels the raw pane does not (e.g.
// the remote-environment picker strips the ✔ marker from the default entry).
func MenuKeystrokeFrom(opts []NumberedOption, chosen string) (string, bool) {
	if len(opts) == 0 {
		return chosen, false
	}
	want := strings.ToLower(strings.TrimSpace(chosen))
	for _, o := range opts {
		if strings.ToLower(o.Label) == want || o.Number == want {
			return o.Number, true
		}
	}
	// A label the operator abbreviated (e.g. "Yes" for "Yes, allow once"):
	// accept a unique prefix match so learned short answers still resolve.
	if key, ok := uniquePrefixMatch(opts, want); ok {
		return key, true
	}
	return chosen, false
}

// DeliverOutbound maps a chosen reply to the numbered-menu digit for
// approval/choice situations and Codex's structural rate-limit error modal.
// Other error retries and idle prompts remain literal free text, so an
// ordinary numbered list cannot hijack their reply into a bare digit.
// agentType is required because the rate-limit shape has Codex-only semantics;
// approval and choice menus remain agent-agnostic. paneContent is the live
// screen. The bool reports whether a menu digit was mapped: false means the
// returned text is free text (callers deciding whether to rewrite literal
// outbound text key off this).
func DeliverOutbound(sitType SituationType, agentType, paneContent, chosen string) (string, bool) {
	menuSituation := sitType == SituationApproval || sitType == SituationChoice ||
		(sitType == SituationError && strings.EqualFold(strings.TrimSpace(agentType), "codex") && CodexRateLimitForm(paneContent))
	if !menuSituation {
		return chosen, false
	}
	return MenuKeystroke(paneContent, chosen)
}

// DeliverKeystroke is DeliverOutbound for callers that only need the text.
func DeliverKeystroke(sitType SituationType, agentType, paneContent, chosen string) string {
	out, _ := DeliverOutbound(sitType, agentType, paneContent, chosen)
	return out
}

// uniquePrefixMatch returns an option's number when exactly one option label
// starts with want; ambiguous or absent prefixes return ("", false).
func uniquePrefixMatch(opts []NumberedOption, want string) (string, bool) {
	if want == "" {
		return "", false
	}
	var hit string
	matches := 0
	for _, o := range opts {
		if strings.HasPrefix(strings.ToLower(o.Label), want) {
			hit = o.Number
			matches++
		}
	}
	if matches == 1 {
		return hit, true
	}
	return "", false
}
