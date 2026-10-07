package domain

import (
	"regexp"
	"strings"
)

// claudeModalCounterRE is the queue counter Claude right-aligns on a paged
// dialog's header line ("Tool use · from the general-purpose agent   2 of 3").
var claudeModalCounterRE = regexp.MustCompile(`\s+\d+ of \d+$`)

// ClaudeModalRegion returns the dialog Claude Code is standing at — every line
// below the LAST plain horizontal rule of the capture, with blank lines and the
// key-hint footer ("Esc to cancel · …") dropped and whitespace collapsed — so
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
	lines := strings.Split(strings.TrimRight(pane, "\n"), "\n")
	top := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if claudeRuleLineRE.MatchString(strings.TrimSpace(lines[i])) {
			top = i
			break
		}
	}
	if top < 0 {
		return "", false
	}
	kept := make([]string, 0, len(lines)-top)
	for _, line := range lines[top+1:] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.Contains(trimmed, "Esc to cancel") {
			continue
		}
		line := strings.Join(strings.Fields(trimmed), " ")
		if len(kept) == 0 {
			line = claudeModalCounterRE.ReplaceAllString(line, "")
		}
		kept = append(kept, line)
	}
	region := strings.Join(kept, "\n")
	if len(ParseNumberedOptions(region)) == 0 {
		return "", false
	}
	return region, true
}

// ClaudeModalAdvanced reports that before and after both show a Claude dialog
// (ClaudeModalRegion) and that it is not the same one — the evidence that an
// answer landed and Claude drew its next queued prompt in place. Either side
// without a dialog answers false: absence of evidence is not an advance.
func ClaudeModalAdvanced(before, after string) bool {
	b, ok := ClaudeModalRegion(before)
	if !ok {
		return false
	}
	a, ok := ClaudeModalRegion(after)
	return ok && a != b
}
