package domain

import (
	"regexp"
	"strings"
)

// numberedOptionRE matches a numbered menu line as agents render it:
// an optional selection caret, then "N." / "N)" / "[N]", then the label.
// The number is captured so the displayed digit is delivered verbatim (a
// menu that starts at 0 or skips a number is honored, not re-indexed).
var numberedOptionRE = regexp.MustCompile(`(?m)^[ \t]*(?:[❯>][ \t]*)?(?:(\d+)[.)]|\[(\d+)\])[ \t]+(\S.*?)[ \t]*$`)

// NumberedOption pairs a menu option's displayed number with its label.
type NumberedOption struct {
	Number string
	Label  string
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

// DeliverKeystroke maps a chosen reply to the numbered-menu digit, but ONLY
// for approval/choice situations. Free-text prompts — idle next-task prompts,
// error-retry commands — are always delivered literally, so a pane that
// happens to contain an ordinary numbered list (e.g. a summary "1. ran tests")
// can never hijack the reply into a bare digit. paneContent is the live
// screen; returns the digit when it maps, else chosen unchanged.
func DeliverKeystroke(sitType SituationType, paneContent, chosen string) string {
	if sitType != SituationApproval && sitType != SituationChoice {
		return chosen
	}
	if key, ok := MenuKeystroke(paneContent, chosen); ok {
		return key
	}
	return chosen
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
