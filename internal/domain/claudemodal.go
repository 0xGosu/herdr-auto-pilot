package domain

import (
	"regexp"
	"strings"
)

// claudeModalCounterRE is the queue counter Claude right-aligns on a paged
// dialog's header line ("Tool use · from the general-purpose agent   2 of 3").
var claudeModalCounterRE = regexp.MustCompile(`\s+\d+ of \d+$`)

// claudeModalCaretRE is the selection caret in front of a numbered option
// ("❯ 1. Yes"). It marks where the cursor rests, not which dialog stands, so it
// is dropped: arrowing through the options, or a digit that only moved the
// caret, must not read as a different dialog.
var claudeModalCaretRE = regexp.MustCompile(`^[❯›>]\s*(\d+[.)]|\[\d+\])`)

// ClaudeModalRegion returns the dialog Claude Code is standing at — every line
// below the LAST plain horizontal rule of the capture, with blank lines, the
// key-hint footer ("Esc to cancel · …", any case) and the selection caret
// dropped and whitespace collapsed — so
// two captures of the same dialog compare equal however the rest of the screen
// moved. ok is false when the capture has no such rule, or when what sits below
// it offers no numbered option: the region is only a dialog when it can be
// answered.
//
// The rule is the dialog's top border. Everything that churns while a dialog
// waits — the spinner, "Waiting for 2 background agents", a finished
// subagent's report — is drawn ABOVE it, which is what makes this region, and
// not the whole capture, the thing to compare. The footer is dropped for the
// same reason: Claude appends hints to it ("ctrl+x ctrl+k twice to stop
// background agents") as background work starts and stops.
//
// So is the header's "N of M" queue counter. It counts the QUEUE, not the
// dialog: it appears when a second requester asks, its total grows as more
// join, its index moves when a queued request settles some other way, and it
// is dropped when the row is too narrow — all while the same dialog stands.
// Kept, a counter-only change would read as an answer that landed and get the
// same prompt answered twice. The cost is deliberate: two consecutive pages
// with IDENTICAL bodies (same tool, same parameters) do not read as an advance,
// which leaves them to the ordinary "still blocked" report — the safe side.
//
// What it exists to see is Claude's paged permission queue. Concurrent
// requesters (parallel subagents) queue their tool approvals behind one dialog
// whose header counts them ("Tool use · from the general-purpose agent  1 of
// 3"); answering one draws the next IN PLACE while herdr keeps reporting the
// agent blocked, so no status event announces it. Verified live 2026-10-07
// against Claude Code 2.1.292.
func ClaudeModalRegion(pane string) (string, bool) {
	m, ok := parseClaudeModal(pane)
	return m.Region, ok
}

// parseClaudeModal is the one reader behind ClaudeModalRegion and
// ParseClaudeMenu, so the region the self-check compares and the one key
// delivery compares can never be cut differently.
func parseClaudeModal(pane string) (ClaudeMenu, bool) {
	lines := strings.Split(strings.TrimRight(pane, "\n"), "\n")
	top := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if claudeRuleLineRE.MatchString(strings.TrimSpace(lines[i])) {
			top = i
			break
		}
	}
	if top < 0 {
		return ClaudeMenu{}, false
	}
	var m ClaudeMenu
	kept := make([]string, 0, len(lines)-top)
	for _, line := range lines[top+1:] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.Contains(strings.ToLower(trimmed), "esc to cancel") {
			continue
		}
		line := strings.Join(strings.Fields(trimmed), " ")
		sub := claudeModalCaretRE.FindStringSubmatch(line)
		line = claudeModalCaretRE.ReplaceAllString(line, "$1")
		if len(kept) == 0 {
			// Caret masked, counter kept: when no title line is drawn the first
			// kept line is an option, and a caret move must not change Header.
			m.Header = line
			line = claudeModalCounterRE.ReplaceAllString(line, "")
		}
		// Only an OPTION line carries the selection caret; a quoted "> 1." in
		// a tool's description is text.
		if sub != nil && m.Caret == "" && numberedOptionRE.MatchString(line) {
			m.Caret = strings.Trim(sub[1], "[].)")
		}
		kept = append(kept, line)
	}
	m.Region = strings.Join(kept, "\n")
	if m.Options = ParseNumberedOptions(m.Region); len(m.Options) == 0 {
		return ClaudeMenu{}, false
	}
	return m, true
}

// ClaudeMenu is a standing Claude dialog reduced to what answering it by KEY
// needs (mcqdeliver.ClaudeMenu): the masked region the self-check compares,
// plus the two things that region deliberately drops.
type ClaudeMenu struct {
	// Region is ClaudeModalRegion's dialog: counter and caret masked.
	Region string
	// Header is the dialog's first line as drawn, whitespace collapsed and
	// caret masked, with the "N of M" queue counter KEPT.
	Header string
	// Caret is the number of the option under the selection caret, "" when
	// none is drawn.
	Caret string
	// Options are the dialog's numbered options.
	Options []NumberedOption
}

// ParseClaudeMenu parses the Claude dialog standing at the bottom of pane (see
// ClaudeModalRegion for what counts as one). A swept multi-tab AGGREGATE is
// refused, as in ClaudeModalAdvanced: its region is the last frame's, never
// what a live read shows.
func ParseClaudeMenu(pane string) (ClaudeMenu, bool) {
	if LooksLikeAggregatedMCQ(pane) {
		return ClaudeMenu{}, false
	}
	return parseClaudeModal(pane)
}

// Offers reports whether digit is one of the dialog's option numbers.
func (m ClaudeMenu) Offers(digit string) bool {
	for _, o := range m.Options {
		if o.Number == digit {
			return true
		}
	}
	return false
}

// SameStanding reports that o is provably the dialog m still standing — the
// only evidence on which a key deliverer may press Enter after a digit.
//
// It is STRICTER than ClaudeModalAdvanced, on purpose, because the two guard
// opposite directions. The self-check must not read a counter-only change as an
// advance (it would answer an un-landed prompt twice), so it masks the counter
// and treats two consecutive pages with identical bodies as "not advanced". Here
// "not advanced" is what licenses an Enter, and an Enter on the NEXT page
// approves it unseen (#564) — so the counter is kept: three subagents asking to
// run the same command draw identical bodies, and only "1 of 3" → "1 of 2"
// tells them apart. Any difference answers false, which costs at worst a
// caret-only build's answer left standing for the self-check to escalate.
// The caret is still masked: a digit that only MOVED it is the same dialog.
func (m ClaudeMenu) SameStanding(o ClaudeMenu) bool {
	return m.Region == o.Region && m.Header == o.Header
}

// ClaudeMenuDigit reports whether a reply to this situation is a Claude menu
// digit — an approval or choice whose reply was mapped to an option's number —
// and so must be pressed as a KEY by mcqdeliver.ClaudeMenu rather than typed
// with a trailing Enter.
//
// Claude Code commits a permission dialog on the digit alone (verified live
// 2026-10-07, 2.1.292). In a paged queue ("1 of 3") it then draws the next
// request in place, and an Enter that follows approves THAT request with its
// default option — unclassified, unaudited, past every safety gate (#564).
//
// Every send path asks it: deliver.Deliver, deliverAutonomousClaimed (the act
// path and the action-review outcome) and the LLM promotion in
// handleLLMOutcome. A new one that does not would type "digit, Enter". Claude
// only: codex's digit binding is unverified.
func ClaudeMenuDigit(sitType SituationType, agentType string, mapped bool) bool {
	return mapped && strings.EqualFold(strings.TrimSpace(agentType), "claude") &&
		(sitType == SituationApproval || sitType == SituationChoice)
}

// ClaudeModalAdvanced reports that before and after both show a Claude dialog
// (ClaudeModalRegion) and that it is not the same one — the evidence that an
// answer landed and Claude drew its next queued prompt in place. Either side
// without a dialog answers false: absence of evidence is not an advance.
//
// So does a swept multi-tab AGGREGATE on either side. Its region is the LAST
// frame's (the Submit tab), which never equals the one frame a live read shows,
// so comparing the two would report every still-standing form as answered. A
// swept form's baseline is compared frame-wise (mcqFormHeldStill), never here.
// The marker test, not the strict parse: a stored excerpt may have been cut.
func ClaudeModalAdvanced(before, after string) bool {
	if LooksLikeAggregatedMCQ(before) || LooksLikeAggregatedMCQ(after) {
		return false
	}
	b, ok := ClaudeModalRegion(before)
	if !ok {
		return false
	}
	a, ok := ClaudeModalRegion(after)
	return ok && a != b
}
