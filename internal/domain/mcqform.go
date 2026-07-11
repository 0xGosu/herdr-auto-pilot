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
// and a footer that names Tab/Arrow navigation. The pane shows ONE question
// at a time; the header is the only signal that more tabs exist.
var (
	mcqTabHeaderRE = regexp.MustCompile(`(?m)^\s*←.*[☐✔].*→\s*$`)
	mcqTabEntryRE  = regexp.MustCompile(`[☐✔]`)
	mcqTabFooterRE = regexp.MustCompile(`(?i)tab/arrow keys to navigate`)
	mcqFooterRE    = regexp.MustCompile(`(?im)^.*enter to select.*$`)
	digitTokenRE   = regexp.MustCompile(`^[1-9]$`)
)

// MultiTabForm reports whether pane content shows the multi-tab MCQ variant
// and how many tabs it has (checkbox entries plus the Submit entry). Both
// the tab header and the Tab/Arrow footer are required, so a narrated
// checkbox list can not false-positive. The LAST header occurrence is the
// live render: a consuming "recent" read can carry earlier renders (or an
// older form) above the current one.
func MultiTabForm(pane string) (tabs int, ok bool) {
	headers := mcqTabHeaderRE.FindAllString(pane, -1)
	if len(headers) == 0 || !mcqTabFooterRE.MatchString(pane) {
		return 0, false
	}
	n := len(mcqTabEntryRE.FindAllString(headers[len(headers)-1], -1))
	if n < 2 {
		return 0, false
	}
	return n, true
}

// ParseDigitSeries parses a space-separated series of menu digits — the
// answer format for multi-tab forms ("1 2 3 2 1", one digit per tab
// including the final Submit tab). A single digit is NOT a series: that is
// an ordinary single-menu answer.
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
	var b strings.Builder
	for i, f := range frames {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "[question %d/%d]\n%s", i+1, len(frames), ExtractMCQForm(f))
	}
	return b.String()
}
