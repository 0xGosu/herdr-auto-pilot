package domain

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Answering agy — docs/designer/agy-support.md §4 and §5 ("Keystrokes").
//
// agy's forms take KEYS, not a submitted reply: a numbered approval or question
// commits on the option's digit ALONE, the trust prompt moves a caret with the
// arrow keys and confirms with Enter, and the plan-artifact review panel takes
// y / n. Every generic send path types the reply and then presses Enter
// (herdr.CLI.submitText), which at an agy menu answers the NEXT screen as well
// — on a two-question form, option 1 of question 2, unseen. So a reply to an
// agy form is only ever typed by the verified keystroke deliverer
// (mcqdeliver.Agy), which maps the reply onto the live form with the helpers
// below, presses one key, and re-reads to prove it landed.

// AgyFormSituation reports whether a reply to this situation answers one of
// agy's forms — an approval or a question — and so must go through the agy
// keystroke deliverer rather than any path that submits text. Every send path
// asks it: a new one that does not would type "digit, Enter" into agy.
func AgyFormSituation(sitType SituationType, agentType string) bool {
	return IsAgy(agentType) && (sitType == SituationApproval || sitType == SituationChoice)
}

// ErrAgyNotAnswerable marks an agy form hap may not answer however long it
// waits: the reply names a question's free-text "Write-in..." row, which asks
// for typed text rather than a choice. It is a verdict about the form, not a
// delivery fault — a retry is refused the same way.
var ErrAgyNotAnswerable = errors.New("hap cannot answer this agy form")

// AgyFormKind names the answer protocol of an agy form.
type AgyFormKind string

const (
	// AgyFormApproval is a numbered shell or file permission prompt: the digit.
	AgyFormApproval AgyFormKind = "approval"
	// AgyFormQuestion is one question of agy's question form: the digit, which
	// commits AND advances to the next question (the last one submits).
	AgyFormQuestion AgyFormKind = "question"
	// AgyFormTrust is the trust-folder prompt: unnumbered rows, up/down to the
	// chosen row, then Enter.
	AgyFormTrust AgyFormKind = "trust"
	// AgyFormReview is the plan-artifact review panel: y approves the focused
	// item, n rejects it. shift+a (approve ALL, unseen) is never sent.
	AgyFormReview AgyFormKind = "review"
)

// AgyForm is an agy form standing at the bottom of a pane, reduced to what
// answering it needs.
type AgyForm struct {
	Kind AgyFormKind
	// Options are the choices in display order. Numbered forms keep their
	// digits; the trust prompt's rows are unnumbered on screen and are numbered
	// 1..n here by position. A question's Write-in row is excluded (WriteIn).
	// Empty for the review panel, which takes y / n.
	Options []NumberedOption
	// Caret is the trust prompt's highlighted row (0-based); -1 when not drawn
	// and for every other kind.
	Caret int
	// WriteIn is a question's free-text row digit, "" when absent.
	WriteIn string
	// identity is what tells this form apart from the one that replaces it: a
	// different command, the next question of the same form, the next review
	// item. The trust prompt's caret is deliberately not part of it — moving the
	// caret does not change which prompt stands.
	identity string
}

// SameAs reports whether g is the same standing form as f (the trust caret
// aside) — the check that a keystroke has NOT yet landed, and that the form on
// screen is still the one a decision was made about.
func (f AgyForm) SameAs(g AgyForm) bool {
	return f.Kind == g.Kind && f.identity == g.identity
}

// ParseAgyForm parses the agy form hap may answer standing at the bottom of
// pane. An approval whose Tab-amend text field is open is NOT one (the
// operator is typing into it, and Esc there cancels the call). Callers must
// gate on agent type agy.
func ParseAgyForm(pane string) (AgyForm, bool) {
	if f, ok := ParseAgyApproval(pane); ok {
		if f.Amending {
			return AgyForm{}, false
		}
		return AgyForm{Kind: AgyFormApproval, Options: f.Options, Caret: -1,
			identity: f.PermissionVerb() + "\x00" + optionIdentity(f.Options)}, true
	}
	if f, ok := ParseAgyMCQ(pane); ok {
		return AgyForm{Kind: AgyFormQuestion, Options: f.Options, WriteIn: f.WriteIn, Caret: -1,
			identity: fmt.Sprintf("%d/%d\x00%s\x00%s", f.Current, f.Total, f.Question, optionIdentity(f.Options))}, true
	}
	if f, ok := ParseAgyTrust(pane); ok {
		opts := make([]NumberedOption, len(f.Options))
		for i, label := range f.Options {
			opts[i] = NumberedOption{Number: strconv.Itoa(i + 1), Label: label}
		}
		return AgyForm{Kind: AgyFormTrust, Options: opts, Caret: f.Selected,
			identity: optionIdentity(opts)}, true
	}
	if f, ok := ParseAgyReview(pane); ok {
		return AgyForm{Kind: AgyFormReview, Caret: -1,
			identity: strconv.Itoa(f.Pending) + "\x00" + strings.Join(f.Items, "\x00")}, true
	}
	return AgyForm{}, false
}

func optionIdentity(opts []NumberedOption) string {
	parts := make([]string, len(opts))
	for i, o := range opts {
		parts[i] = o.Number + "." + o.Label
	}
	return strings.Join(parts, "\x00")
}

// agyReviewReplies maps a folded reply onto the review panel's key.
var agyReviewReplies = map[string]string{
	"y": "y", "yes": "y", "approve": "y", "approved": "y", "accept": "y",
	"n": "n", "no": "n", "reject": "n", "rejected": "n", "deny": "n",
}

// AgyAnswerKey maps reply — an option label (folded like every menu reply), a
// unique prefix of one, or its digit — onto the key that answers f: the
// option's digit for a numbered form or the trust prompt (whose deliverer turns
// the row number into arrow presses), "y" / "n" for the review panel.
//
// A reply that names no offered option is refused, never typed: agy would take
// the letters as nothing and a later Enter would commit the caret's row. A
// reply naming a question's Write-in row wraps ErrAgyNotAnswerable.
func AgyAnswerKey(f AgyForm, reply string) (string, error) {
	switch f.Kind {
	case AgyFormReview:
		if key, ok := agyReviewReplies[FoldMenuText(reply)]; ok {
			return key, nil
		}
		return "", fmt.Errorf("%q is neither an approval (y) nor a rejection (n) of the plan artifact", reply)
	case AgyFormQuestion:
		if t := strings.TrimSpace(reply); f.WriteIn != "" && (t == f.WriteIn || agyWriteInRE.MatchString(t)) {
			return "", fmt.Errorf("%w: %q is the free-text Write-in row, which takes typed text", ErrAgyNotAnswerable, reply)
		}
	}
	digit, ok := MenuKeystrokeFrom(f.Options, reply)
	if !ok {
		return "", fmt.Errorf("%q matches none of the options the agy form is offering", reply)
	}
	return digit, nil
}

// agyModePlaceholderRE matches the placeholder agy shows in an EMPTY composer
// outside the default mode ("> Plan mode: research & plan only (shift+tab to
// cycle)"). It is not a draft: nothing has been typed.
var agyModePlaceholderRE = regexp.MustCompile(`^>\s*(?:Plan|Accept-edits) mode: .*\(shift\+tab to cycle\)$`)

// AgyComposerReady proves agy is parked at an EMPTY composer ready for a
// message — the one state in which typing a hand-out or a free-text reply is
// safe. herdr reports every agy modal as idle, so its status cannot answer this
// (docs/designer/agy-support.md §4.1). From the bottom up it requires:
//
//  1. the status bar with its "? for shortcuts" left token — a draft removes
//     it, and a working turn, a modal or the slash popup replaces it with
//     "esc to cancel";
//  2. the composer sandwich directly above it: a rule, a caret line that is
//     empty or exactly a mode placeholder, a rule — so a form or panel drawn
//     below the composer, which pushes the status bar away, is refused;
//  3. no held screen — in particular the feedback survey, the one overlay seen
//     standing above a ready composer, where a typed digit answers the SURVEY.
//
// Absence of any piece is "not ready", never "unknown so go ahead".
func AgyComposerReady(pane string) bool {
	lines := agyDropBackgroundStrip(
		trimTrailingBlank(strings.Split(strings.ReplaceAll(pane, "\r", ""), "\n")))
	n := len(lines)
	if n < 4 {
		return false
	}
	if !strings.HasPrefix(strings.TrimSpace(lines[n-1]), "? for shortcuts") || !agyStatusBarLine(lines[n-1]) {
		return false
	}
	caret := strings.TrimSpace(lines[n-3])
	if !agyRuleLineRE.MatchString(strings.TrimSpace(lines[n-2])) ||
		!agyRuleLineRE.MatchString(strings.TrimSpace(lines[n-4])) ||
		(caret != ">" && !agyModePlaceholderRE.MatchString(caret)) {
		return false
	}
	_, held := AgyHeldForm(pane)
	return !held
}

// agyComposerScanLimit is the FLOOR on the search for the composer's opening
// rule. A queued multi-line hand-out renders as several lines between the rules,
// so the block cannot be read at a fixed offset the way AgyComposerReady reads
// an empty one — but scanning the whole capture would happily pair the closing
// rule with one agy drew between two earlier turns. A caller that knows what it
// sent widens the window with AgyDraftScanLimit; 20 lines is what is left for a
// caller that does not.
const agyComposerScanLimit = 20

// agyComposerScanCeiling caps AgyDraftScanLimit. Past it the risk of pairing
// with an inter-turn rule outweighs the reach, and the cost of stopping is only
// an AgyDraftUnreadable verdict — which callers already treat as the cautious
// answer, not as "nothing is there".
const agyComposerScanCeiling = 120

// agyDraftWrapWidth is the narrowest pane AgyDraftScanLimit budgets for. agy
// hard-wraps the composer at the terminal width, which the capture does not
// report, so the line count has to be estimated from the text alone and the
// estimate must be generous: too FEW lines is what makes the opening rule
// unreachable, which is the failure this constant exists to prevent.
const agyDraftWrapWidth = 40

// agyDraftScanSlack is added to the estimate to cover agy's own framing of a
// draft (indentation, a blank continuation line).
const agyDraftScanSlack = 4

// agyDraftMatchRunes is how much of the composer must match what was sent for
// the draft to be attributed to hap. Both a FLOOR and a cap: a shorter draft is
// only evidence when it is the whole of what was sent (see AgyDraftMatches).
// Long enough that an operator's own sentence cannot collide by accident, short
// enough to survive agy wrapping or truncating a long hand-out.
const agyDraftMatchRunes = 24

// AgyDraftVerdict is what reading agy's composer back established.
type AgyDraftVerdict int

const (
	// AgyDraftNone: no unsent draft is on screen. An empty composer, a mode
	// placeholder, a working turn, a form — every screen that positively is
	// not a message waiting to be sent.
	AgyDraftNone AgyDraftVerdict = iota
	// AgyDraftPresent: the returned text is sitting unsent in the composer.
	AgyDraftPresent
	// AgyDraftUnreadable: a composer is on screen and its closing rule is
	// directly above the bottom chrome, but no opening rule is within reach —
	// so the block cannot be read and the draft cannot be compared.
	//
	// This is NOT the same as AgyDraftNone, and collapsing the two is the bug
	// it exists to prevent. An EMPTY composer finds its opening rule at n-4 on
	// the first step of the scan, always; so does a one-line draft. Reaching
	// the end of the window means something LARGE is in the composer —
	// overwhelmingly a wrapped hand-out that has just been queued, which is
	// exactly the case a caller must not read as "delivered".
	AgyDraftUnreadable
)

// AgyDraftScanLimit is the scan window a caller should allow when it knows what
// it sent: enough lines for that text to wrap at a pessimistically narrow pane
// width, floored at agyComposerScanLimit and capped at agyComposerScanCeiling.
//
// A hand-out is a rendered task prompt — boilerplate plus the task text — and
// wraps well past 20 lines for anything but a one-liner, so a fixed window
// answers AgyDraftUnreadable for precisely the LARGE hand-outs, i.e. the common
// case. Widening it is what keeps that verdict rare enough to be a real signal.
func AgyDraftScanLimit(sent string) int {
	lines := 0
	for _, seg := range strings.Split(strings.ReplaceAll(sent, "\r", ""), "\n") {
		lines += utf8.RuneCountInString(seg)/agyDraftWrapWidth + 1
	}
	limit := lines + agyDraftScanSlack
	if limit < agyComposerScanLimit {
		return agyComposerScanLimit
	}
	if limit > agyComposerScanCeiling {
		return agyComposerScanCeiling
	}
	return limit
}

// AgyComposerDraft returns the text agy is holding UNSENT in its composer.
//
// It answers what AgyComposerReady cannot. That one proves the composer is
// EMPTY, and lumps everything else together as "not ready" — a working turn, a
// modal and a draft are all equally unsafe to type into. After a hand-out those
// stop being interchangeable: agy QUEUES text typed during a turn in its
// composer rather than acting on it (observed live 2026-09-12), and the queued
// copy fires whenever the turn ends. So a hand-out still sitting here has NOT
// been taken up, while a working turn means it has — and only reading the
// composer back tells the two apart.
//
// The block is read BETWEEN the composer's two rules rather than at a fixed
// offset, because a multi-line hand-out wraps across several lines. The bottom
// chrome line is required but its left token is not: agy DROPS "? for shortcuts"
// while a draft stands, which is precisely the state this looks for.
//
// A held screen returns AgyDraftNone — something else is on top, so the caret
// line is not simply an unsent message.
//
// scanLimit bounds how far above the closing rule the opening one is looked
// for; callers that know what they sent pass AgyDraftScanLimit(sent). Running
// out of window is AgyDraftUnreadable, never AgyDraftNone: see that constant.
func AgyComposerDraft(pane string, scanLimit int) (string, AgyDraftVerdict) {
	if scanLimit < agyComposerScanLimit {
		scanLimit = agyComposerScanLimit
	}
	lines := trimTrailingBlank(strings.Split(strings.ReplaceAll(pane, "\r", ""), "\n"))
	n := len(lines)
	if n < 4 || !agyBottomChromeLine(lines[n-1]) ||
		!agyRuleLineRE.MatchString(strings.TrimSpace(lines[n-2])) {
		return "", AgyDraftNone
	}
	open := -1
	for i := n - 3; i >= 0 && i >= n-3-scanLimit; i-- {
		if agyRuleLineRE.MatchString(strings.TrimSpace(lines[i])) {
			open = i
			break
		}
	}
	if open < 0 {
		// The composer's closing rule is there but its opening rule is not
		// within reach — the block is too tall to read, not absent.
		return "", AgyDraftUnreadable
	}
	if open >= n-3 {
		return "", AgyDraftNone
	}
	block := lines[open+1 : n-2]
	first := strings.TrimSpace(block[0])
	// The nearest rule above the closing one IS the composer's opening rule
	// whenever a draft stands, so a first line that is not a caret line means
	// this is not a composer at all — the guard that keeps a widened window
	// from pairing with a rule agy drew between two earlier turns.
	if !strings.HasPrefix(first, ">") || agyModePlaceholderRE.MatchString(first) {
		return "", AgyDraftNone
	}
	var b strings.Builder
	b.WriteString(strings.TrimSpace(strings.TrimPrefix(first, ">")))
	for _, l := range block[1:] {
		if t := strings.TrimSpace(l); t != "" {
			b.WriteString(" ")
			b.WriteString(t)
		}
	}
	text := strings.TrimSpace(b.String())
	if text == "" {
		return "", AgyDraftNone
	}
	if _, held := AgyHeldForm(pane); held {
		return "", AgyDraftNone
	}
	return text, AgyDraftPresent
}

// agyBottomChromeLine matches the line agy paints at the very bottom: its status
// bar, or — while a draft stands, which removes the bar's left token — the bare
// right-aligned model segment.
func agyBottomChromeLine(line string) bool {
	if agyStatusBarLine(line) {
		return true
	}
	trimmed := strings.TrimSpace(line)
	return trimmed != "" && agyModelSegmentRE.MatchString(trimmed)
}

// AgyDraftMatches reports whether the text agy is holding in its composer is the
// hand-out that was just sent, rather than something the operator typed.
//
// A queued hand-out is typed from its FIRST character, so the composer holds a
// prefix of what was sent — hence HasPrefix rather than a containment test,
// which a two-word operator draft could satisfy by coincidence. Only the head is
// compared, because agy wraps a long message and the composer shows only what
// fits.
//
// ALL whitespace is dropped from both sides before comparing, rather than
// collapsed to single spaces. A terminal wrap breaks the text wherever the
// column runs out — mid-word as readily as at a space — and the composer block
// is rejoined line by line, so a collapsed comparison inserts a separator that
// is not in what was sent ("…run the su" + " " + "ite…") and a genuinely queued
// hand-out then matches nothing. Dropping whitespace entirely makes the
// comparison independent of where the wrap fell; 24 runes of non-whitespace is
// still far more than an operator's opening words could collide with.
//
// The window is a FLOOR as well as a cap: a draft shorter than it is compared in
// full only when it IS the whole of what was sent. Otherwise one or two
// characters the operator typed just after the send ("N" against "Next task: …")
// would prefix-match and their draft would be attributed to hap. The operator's
// literal suggestion — refuse whenever the head is shorter than the window —
// would instead refuse a short hand-out that really was queued, which is the
// costly direction; hence the min() rather than a flat floor.
//
// Getting this wrong is not symmetric, and it is deliberately biased: a draft
// wrongly attributed to hap strands a task at "[-]" until an operator looks,
// while one wrongly attributed to the operator falls back to the ledger path —
// which is only safe when the item really was taken up, so this refuses
// whenever it cannot tell rather than guessing either way.
func AgyDraftMatches(draft, sent string) bool {
	// The floor is measured on the draft as it READS — whitespace collapsed to
	// single spaces — because it answers "how much did somebody have to type".
	// The comparison is on whitespace-stripped text because it answers "where
	// did the wrap fall", which must not change the answer either way.
	typed := []rune(strings.Join(strings.Fields(draft), " "))
	d := []rune(agyStripSpace(draft))
	s := []rune(agyStripSpace(sent))
	if len(d) == 0 || len(s) == 0 {
		return false
	}
	if len(typed) < agyDraftMatchRunes && len(d) < len(s) {
		return false
	}
	head := d
	if len(head) > agyDraftMatchRunes {
		head = head[:agyDraftMatchRunes]
	}
	return strings.HasPrefix(string(s), string(head))
}

// agyStripSpace removes every whitespace rune, so a comparison cannot depend on
// where a terminal wrap fell. See AgyDraftMatches.
func agyStripSpace(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}
