package domain

import (
	"fmt"
	"regexp"
	"strings"
)

// Claude's AskUserQuestion / plan-mode multi-tab MCQ form renders a tab
// header row between arrows, one checkbox per question plus a final Submit
// entry, e.g.:
//
//	←  ☐ New name  ☐ Rename depth  ☐ Config compat  ☐ Conciseness  ✔ Submit  →
//
// and a footer that names tab navigation ("Tab/Arrow keys to navigate" on
// older builds, "Tab to switch questions" since Claude Code v2.1.207). The
// pane shows ONE question at a time; the header is the only signal that more
// tabs exist.
var (
	// The tab header marks each question ☐ (unanswered) or ☒ (answered) plus a
	// final ✔ Submit. ☒ must be counted too — an operator (or the daemon) may
	// answer a tab before the form is (re)captured, and missing ☒ undercounts
	// the tabs (verified live: a partially-answered 3-tab form read as 2).
	mcqTabHeaderRE = regexp.MustCompile(`(?m)^\s*←.*[☐☒✔].*→\s*$`)
	mcqTabEntryRE  = regexp.MustCompile(`[☐☒✔]`)
	mcqTabFooterRE = regexp.MustCompile(`(?i)(tab/arrow keys to navigate|tab to switch questions)`)
	mcqFooterRE    = regexp.MustCompile(`(?im)^.*enter to select.*$`)
	// digitTokenRE matches one per-tab answer token: a single menu digit, or —
	// for a multi-select tab that toggles several options — a comma-separated
	// set of digits ("1,3"). There is still exactly ONE token per tab, so the
	// len(tokens)==TabCount guards hold whether or not a tab is multi-select.
	digitTokenRE = regexp.MustCompile(`^[1-9](,[1-9])*$`)
	// mcqTabCaretRE matches the option the selection caret currently sits on
	// ("❯ 2. Square" -> "2"). Delivery must confirm the caret reached the
	// intended option before pressing Enter, or Enter would commit whatever
	// option the caret happened to rest on.
	mcqTabCaretRE = regexp.MustCompile(`(?m)^[ \t]*❯[ \t]*(\d+)[.)][ \t]+`)
)

// mcqSubmitScreenRE matches the final Submit tab's confirmation body. That
// tab keeps the `←…✔ Submit…→` header but DROPS the tab-navigation footer
// (issue #95), so the footer alone can not stand for "this is still the live
// multi-tab form". The "Ready to submit your answers?" prompt is present
// whether or not every question is answered (the "⚠ You have not answered all
// questions" warning is conditional), and it is line-anchored so narration
// can not trip it.
var mcqSubmitScreenRE = regexp.MustCompile(`(?im)^\s*ready to submit your answers\?\s*$`)

// mcqSingleFooterRE matches Claude's single-question selection footer — the
// "Enter to select" line plus its navigation tail (a "·" separator or the
// word "navigate"). The tail keeps an agent merely narrating "press enter to
// select" (fzf-style help text in dev output) from reading as a live prompt.
// The single-question form carries no tab header, so this footer is the only
// structural signal it is a live menu.
var mcqSingleFooterRE = regexp.MustCompile(`(?im)^.*enter to select.*(·|\bnavigate\b).*$`)

// ClaudeMCQForm reports whether pane content shows any of Claude Code's
// on-screen MCQ selection prompts: the multi-tab AskUserQuestion form (a tab
// header plus its navigation footer, via MultiTabForm) or the single-question
// form (an "Enter to select … navigate" footer). This is the choice-
// classification signal for claude, replacing brittle numbered-line matching
// that any narrated list would trip.
func ClaudeMCQForm(pane string) bool {
	if _, ok := MultiTabForm(pane); ok {
		return true
	}
	return mcqSingleFooterRE.MatchString(pane)
}

// ParseMCQForm recognizes the agent's structural MCQ form and returns the
// navigation protocol state. It deliberately remains agent-scoped: identical
// text from another agent is narration, not a license to send keystrokes.
func ParseMCQForm(agentType, pane string) (MCQFormState, bool) {
	switch {
	case strings.EqualFold(agentType, "codex"):
		return CodexMCQForm(pane)
	case strings.EqualFold(agentType, "claude"):
		if tabs, ok := MultiTabForm(pane); ok {
			return MCQFormState{Kind: MCQClaudeTabs, AnswerCount: tabs}, true
		}
		if ClaudeMCQForm(pane) {
			return MCQFormState{AnswerCount: 1}, true
		}
	}
	return MCQFormState{}, false
}

// MultiTabForm reports whether pane content shows the multi-tab MCQ variant
// and how many tabs it has (checkbox entries plus the Submit entry). The tab
// header is always required; alongside it the pane must carry EITHER the
// tab-navigation footer (the question tabs) OR the Submit confirmation body
// (the final tab drops the footer — issue #95). Requiring one of the two
// keeps a narrated checkbox list from false-positiving. The LAST header
// occurrence is the live render: a consuming "recent" read can carry earlier
// renders (or an older form) above the current one.
func MultiTabForm(pane string) (tabs int, ok bool) {
	headers := mcqTabHeaderRE.FindAllString(pane, -1)
	if len(headers) == 0 {
		return 0, false
	}
	if !mcqTabFooterRE.MatchString(pane) && !mcqSubmitScreenRE.MatchString(pane) {
		return 0, false
	}
	n := len(mcqTabEntryRE.FindAllString(headers[len(headers)-1], -1))
	if n < 2 {
		return 0, false
	}
	return n, true
}

// ClaudeTabForm parses the LIVE multi-tab render into the state a delivery
// keystroke can be verified against: how many question tabs still owe an
// answer, and which option the caret sits on.
//
// It exists because Claude renders the SAME form with two different key
// protocols, decided per tab by whether its options carry a preview
// (verified live 2026-07-16, Claude Code / Haiku 4.5):
//
//   - plain options ("1. Apple" / "2. Banana"): the DIGIT selects and commits,
//     auto-advancing to the next tab.
//   - options with previews (option list left, preview box right, "Notes:
//     press n to add notes"): the digit only MOVES THE CARET, exactly like
//     ↑/↓ — ENTER is what commits and advances.
//
// The footer is identical in both ("Enter to select · ↑/↓ to navigate · …")
// and never advertises digits, so the binding cannot be told apart by
// rendering alone — and a form can mix the two (a preview form's generated
// Submit tab renders plain). Delivery therefore presses the digit, re-reads,
// and only presses Enter if the answer did not commit; see internal/mcqdeliver.
// Blind digit-only delivery is a silent no-op on preview tabs.
func ClaudeTabForm(pane string) (MCQFormState, bool) {
	total, ok := MultiTabForm(pane)
	if !ok {
		return MCQFormState{}, false
	}
	headers := mcqTabHeaderRE.FindAllStringIndex(pane, -1)
	last := headers[len(headers)-1]
	// An answered tab renders ☒ and the Submit entry renders ✔, so ☐ alone
	// counts the tabs that still owe an answer — the Claude analogue of
	// Codex's "(N unanswered)" counter, and the signal that tells delivery
	// whether a keystroke actually committed.
	state := MCQFormState{
		Kind:        MCQClaudeTabs,
		AnswerCount: total,
		Unanswered:  strings.Count(pane[last[0]:last[1]], "☐"),
	}
	// Scope the caret search to the live form: from the last header down to
	// its footer, so a stale render (or the composer) can not supply it.
	region := pane[last[0]:]
	if end := mcqFooterRE.FindStringIndex(region); end != nil {
		region = region[:end[0]]
	}
	if m := mcqTabCaretRE.FindStringSubmatch(region); m != nil {
		state.SelectedOption = m[1]
	}
	state.Question = claudeTabQuestion(region)
	return state, true
}

// claudeTabQuestion returns the tab's question line — the first non-empty line
// after the tab header. It is the only per-tab identity the render exposes (the
// header carries no "1/3" index), and unlike the option lines it is stable
// while the caret moves: in the preview layout the option lines carry the
// preview box, whose CONTENT changes with the focused option.
func claudeTabQuestion(region string) string {
	lines := strings.Split(region, "\n")
	for _, line := range lines[1:] { // lines[0] is the header itself
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

// ParseDigitSeries parses the space-separated per-tab answer tokens for a
// multi-tab form ("1 2 3 2 1", one token per tab including the final Submit
// tab). A token is a menu digit or — for a multi-select tab — a comma-
// separated set of digits to toggle ("1,3"). There is exactly one token per
// tab, so callers that gate on len(tokens)==TabCount stay correct. A single
// token is NOT a series: that is an ordinary single-menu answer. Delivery
// uses ParseTabSelections to expand each token into its individual digits.
func ParseDigitSeries(s string) ([]string, bool) {
	fields := strings.Fields(s)
	if len(fields) < 2 {
		return nil, false
	}
	for _, f := range fields {
		if !digitTokenRE.MatchString(f) {
			return nil, false
		}
	}
	return fields, true
}

// ParseTabSelections expands ParseDigitSeries into per-tab digit lists: each
// tab's token is comma-split into the option digits to press, with duplicates
// removed in first-seen order ("1 1,3 2" -> [["1"],["1","3"],["2"]]). The
// number of groups equals the tab count, so len==TabCount guards still hold.
func ParseTabSelections(s string) ([][]string, bool) {
	fields, ok := ParseDigitSeries(s)
	if !ok {
		return nil, false
	}
	groups := make([][]string, len(fields))
	for i, f := range fields {
		seen := make(map[string]bool)
		var digs []string
		for _, d := range strings.Split(f, ",") {
			if !seen[d] {
				seen[d] = true
				digs = append(digs, d)
			}
		}
		groups[i] = digs
	}
	return groups, true
}

// ExtractMCQForm returns just the form region of one pane frame: from the
// LAST tab header line (the live render — earlier ones are stale re-renders
// in the scrollback) through the option list, stopping before the
// navigation footer. Scrollback above the form is dropped so aggregating N
// frames does not repeat it N times. A frame without the header is returned
// unchanged.
func ExtractMCQForm(frame string) string {
	locs := mcqTabHeaderRE.FindAllStringIndex(frame, -1)
	if len(locs) == 0 {
		return frame
	}
	region := frame[locs[len(locs)-1][0]:]
	if end := mcqFooterRE.FindStringIndex(region); end != nil {
		region = region[:end[0]]
	}
	return strings.TrimRight(region, "\n \t")
}

// ExtractAgentMCQForm dispatches scrollback trimming to the form variant.
func ExtractAgentMCQForm(kind MCQKind, frame string) string {
	if kind == MCQCodexQuestions {
		return ExtractCodexMCQForm(frame)
	}
	return ExtractMCQForm(frame)
}

// FirstMCQQuestion returns the frame-1 form region embedded in an
// AggregateMCQFrames result. Delivery-time staleness checks compare it to
// the live pane: two forms with the SAME tab count but different questions
// must never receive each other's answers.
func FirstMCQQuestion(aggregate string) string {
	block := aggregate
	if i := strings.Index(block, "\n\n[question 2/"); i >= 0 {
		block = block[:i]
	}
	if i := strings.Index(block, "]\n"); i >= 0 && strings.HasPrefix(block, "[question ") {
		block = block[i+2:]
	}
	return block
}

// AggregateMCQFrames merges the per-tab frames captured by the daemon's
// Right-arrow sweep into one content block — question i/N plus its options,
// in order. This aggregate (not any single frame) feeds the signature, the
// escalation body, and the LLM consult context.
func AggregateMCQFrames(frames []string) string {
	return AggregateAgentMCQFrames(MCQClaudeTabs, frames)
}

// AggregateAgentMCQFrames merges every answer frame in form order.
func AggregateAgentMCQFrames(kind MCQKind, frames []string) string {
	var b strings.Builder
	for i, f := range frames {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "[question %d/%d]\n%s", i+1, len(frames), ExtractAgentMCQForm(kind, f))
	}
	return b.String()
}
