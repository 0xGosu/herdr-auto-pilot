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
// Pass a nil/empty chosen to demand an all-unchecked frame: every checked box
// then reads as foreign. Callers with no decided answer (capture) do not ask
// this question at all — with nothing to compare against, refusing would
// strand hap's own half-delivered forms; they record and let the delivery-time
// call decide.
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
// option — by label (compared through FoldMenuText: case, whitespace and
// typographic punctuation folded away), by a unique folded prefix of one, or by
// an already-numeric selection — MenuKeystroke returns that option's digit and
// true. A match that is ambiguous across two DIFFERENT option numbers is
// refused rather than guessed.
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
	// Exact match is unique-or-refuse, like the prefix match below. One capture
	// can hold the same menu twice — a scrolled pane, or an earlier render still
	// in the buffer — and if the two renders order their options differently,
	// returning the FIRST hit would deliver a digit from the stale render while
	// reporting a clean mapping. Two hits carrying the same number are the same
	// option seen twice and stay fine.
	want := FoldMenuText(chosen)
	hit, hits := "", 0
	for _, o := range opts {
		if FoldMenuText(o.Label) == want || o.Number == want {
			if hit != o.Number {
				hit, hits = o.Number, hits+1
			}
		}
	}
	if hits == 1 {
		return hit, true
	}
	if hits > 1 {
		return chosen, false
	}
	// A label the operator abbreviated (e.g. "Yes" for "Yes, allow once"):
	// accept a unique prefix match so learned short answers still resolve.
	if key, ok := uniquePrefixMatch(opts, want); ok {
		return key, true
	}
	return chosen, false
}

// typographicFolds maps the presentation glyphs an agent TUI renders onto the
// ASCII an operator, a task file or an LLM actually types. Comparison only —
// nothing that is DELIVERED is folded (a menu answer is always the option's own
// digit, and free text is sent byte-for-byte).
//
// Verified live 2026-07-31 against Claude Code 2.1.220: its Bash approval
// renders option 2 as "Yes, and don't ask again for: npm *" with U+2019, while
// every source of a chosen reply — a learned rule, an LLM suggestion, this
// repo's own classifier fixtures — writes the ASCII "don't". Comparing raw, the
// label matched nothing, MenuKeystroke fell through to the literal text, and
// the literal + Enter committed whatever option the caret rested on: option 1,
// silently, with a success exit code. Folding is what makes the intended option
// match; UnmatchedMenuReply is what catches the cases folding cannot.
// The set is deliberately narrow — only glyphs that are a RENDERING of the
// ASCII beside them. A backtick and an acute accent are not folded even though
// they look apostrophe-ish: each carries its own meaning (a code span, an
// accented letter), and every widening here brings two genuinely different
// labels one character closer to comparing equal. An ellipsis is not folded
// either: a label truncated with … cannot prefix-match a full one in either
// direction, so the fold would add collision surface and buy nothing.
// Unicode spaces need no entries — FoldMenuText's strings.Fields already
// collapses every unicode.IsSpace rune, NBSP included.
var typographicFolds = strings.NewReplacer(
	"‘", "'", "’", "'", "‚", "'", "‛", "'", "′", "'",
	"“", `"`, "”", `"`, "„", `"`, "″", `"`,
	"‐", "-", "‑", "-", "‒", "-", "–", "-", "—", "-", "―", "-", "−", "-",
)

// FoldMenuText normalizes a menu label or a chosen reply so the two compare on
// meaning rather than on typography: typographic punctuation folds to ASCII,
// whitespace runs collapse, and case is dropped.
func FoldMenuText(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(typographicFolds.Replace(s)), " "))
}

// MenuSituation reports whether sitType is answered by pressing an option on a
// numbered menu rather than by typing free text — the same gate DeliverOutbound
// applies before mapping.
func MenuSituation(sitType SituationType, agentType, paneContent string) bool {
	return sitType == SituationApproval || sitType == SituationChoice ||
		(sitType == SituationError && strings.EqualFold(strings.TrimSpace(agentType), "codex") && CodexRateLimitForm(paneContent))
}

// UnmatchedMenuReply reports that paneContent presents a numbered menu for this
// situation and chosen matches NONE of its options — the one state in which a
// reply must never be delivered.
//
// It exists because the fallback for "no digit could be mapped" is to type the
// reply literally, and on a standing menu that is not a harmless no-op: the
// agent ignores the letters and the trailing Enter COMMITS the option the caret
// rests on, which is the first one. Verified live 2026-07-31 against Claude Code
// 2.1.220 — sending an unmatched label at a Bash approval ran the command under
// plain "Yes" and answered nothing the reply asked for, reporting success.
//
// It is deliberately false whenever the pane shows no numbered options at all:
// a free-text prompt (a typed reply, an error-retry command) is delivered
// literally and always has been.
func UnmatchedMenuReply(sitType SituationType, agentType, paneContent, chosen string) bool {
	if !MenuSituation(sitType, agentType, paneContent) {
		return false
	}
	opts := ParseNumberedOptions(paneContent)
	if len(opts) == 0 {
		return false
	}
	_, mapped := MenuKeystrokeFrom(opts, chosen)
	return !mapped
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
	if !MenuSituation(sitType, agentType, paneContent) {
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
// starts with want; ambiguous or absent prefixes return ("", false). want is
// already folded by the caller, so labels are folded here to match.
func uniquePrefixMatch(opts []NumberedOption, want string) (string, bool) {
	if want == "" {
		return "", false
	}
	var hit string
	matches := 0
	for _, o := range opts {
		if strings.HasPrefix(FoldMenuText(o.Label), want) {
			hit = o.Number
			matches++
		}
	}
	if matches == 1 {
		return hit, true
	}
	return "", false
}
