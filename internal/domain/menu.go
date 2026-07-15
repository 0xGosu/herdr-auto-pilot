package domain

import (
	"regexp"
	"strings"
)

// numberedOptionRE matches a numbered menu line as agents render it:
// an optional selection caret, then "N." / "N)" / "[N]", then the label.
// The number is captured so the displayed digit is delivered verbatim (a
// menu that starts at 0 or skips a number is honored, not re-indexed).
var numberedOptionRE = regexp.MustCompile(`(?m)^[ \t]*(?:[❯›>][ \t]*)?(?:(\d+)[.)]|\[(\d+)\])[ \t]+(\S.*?)[ \t]*$`)

// checkboxLabelRE matches the leading `[ ]` / `[x]` checkbox marker that a
// Claude multi-SELECT question renders in front of each toggleable option
// (e.g. "1. [ ] Auto-sends" parses to label "[ ] Auto-sends"). The capture
// group is the box contents: a space (unchecked) or x/X (checked).
var checkboxLabelRE = regexp.MustCompile(`^\[([ xX])\]`)

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
			states[o.Number] = m[1] == "x" || m[1] == "X"
		}
	}
	return states
}

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
	opts := ParseNumberedOptions(content)
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

// DeliverOutbound maps a chosen reply to the numbered-menu digit, but ONLY
// for approval/choice situations. Free-text prompts — idle next-task prompts,
// error-retry commands — are always delivered literally, so a pane that
// happens to contain an ordinary numbered list (e.g. a summary "1. ran tests")
// can never hijack the reply into a bare digit. paneContent is the live
// screen. The bool reports whether a menu digit was mapped: false means the
// returned text is free text (callers deciding whether to rewrite literal
// outbound text key off this).
func DeliverOutbound(sitType SituationType, paneContent, chosen string) (string, bool) {
	if sitType != SituationApproval && sitType != SituationChoice {
		return chosen, false
	}
	return MenuKeystroke(paneContent, chosen)
}

// DeliverKeystroke is DeliverOutbound for callers that only need the text.
func DeliverKeystroke(sitType SituationType, paneContent, chosen string) string {
	out, _ := DeliverOutbound(sitType, paneContent, chosen)
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
