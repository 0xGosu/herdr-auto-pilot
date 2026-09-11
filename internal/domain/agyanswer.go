package domain

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
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
	lines := trimTrailingBlank(strings.Split(strings.ReplaceAll(pane, "\r", ""), "\n"))
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
