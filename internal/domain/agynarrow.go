package domain

import "regexp"

// Preferring the narrowest approval option.
//
// Measured on a live run (2026-09-12): hap's LLM chose agy's "Yes, and always
// allow in this conversation for commands that start with X" at confidence
// 98-99 on nearly every approval. Answering that does not approve a command —
// it pre-authorises a command PREFIX for the rest of the conversation, so every
// later command matching it runs with no prompt at all. It never reaches a
// pane, so it never reaches the classifier, the never-auto screen, or the
// operator. A short prefix ("ln", "git", "pwd") disables the safety net for a
// whole class of commands, silently.
//
// The correction here is a PREFERENCE, not a refusal, and that distinction is
// deliberate. Refusing a widening option only escalates it — hap would not then
// pick a narrower one — so a refusal alone turns most approvals into
// escalations, which is the queue-flooding failure this run already suffered.
// Substituting the narrow option answers the prompt and keeps the agent moving,
// which is why this is on by default while the hard refusal
// (safety.enable_never_auto_action_seeds) stays opt-in beside it.

var (
	// agyWideningRE matches an option that grants MORE than this one request.
	// Anchored on the conjunction ("and always allow"), never on a bare
	// "allow": a file prompt's plain "Yes, allow access" is the narrow answer
	// and must not read as widening.
	agyWideningRE = regexp.MustCompile(`(?i)\band\s+always\s+allow\b|\bdon'?t\s+ask\s+(me\s+)?again\b|\bpersist\s+to\s+settings\.json\b|\ballow\s+all\b|\byes\s+to\s+all\b`)
	// agyAffirmativeRE matches an option that GRANTS the request. Used to keep
	// the substitution from ever turning an approval into a refusal.
	agyAffirmativeRE = regexp.MustCompile(`(?i)^(yes|allow|approve|proceed)\b`)
	// agyRefusalRE is checked as well, because "Allow" appears inside some
	// denial labels ("No, deny access to this file").
	agyRefusalRE = regexp.MustCompile(`(?i)^(no|deny|reject|cancel|skip)\b`)
)

// AgyWideningOption reports whether an option label grants permission beyond
// the single request on screen.
func AgyWideningOption(label string) bool {
	return agyWideningRE.MatchString(label)
}

// AgyGrantingOption reports whether an option label APPROVES the request, as
// opposed to declining it.
func AgyGrantingOption(label string) bool {
	l := FoldMenuText(label)
	return agyAffirmativeRE.MatchString(l) && !agyRefusalRE.MatchString(l)
}

// NarrowestAgyApproval substitutes the narrowest granting option when chosen is
// a scope-widening one, returning the label to answer with and whether it
// changed.
//
// chosen is resolved through the form's own options FIRST, so it works whether
// the caller holds the option's label or its digit — the LLM answers an
// approval with select_options (a number), while a learned rule holds a label.
//
// It substitutes only when exactly ONE granting non-widening option is offered.
// With none there is nothing narrower to prefer; with several, which one
// "satisfies the request" is a judgement this cannot make from labels alone, and
// guessing would answer a prompt differently from what any human reviewed. In
// both cases the original answer stands and the ordinary safety controls — the
// action rules among them — decide what happens to it.
func NarrowestAgyApproval(form AgyForm, chosen string) (string, bool) {
	if form.Kind != AgyFormApproval || len(form.Options) == 0 {
		return chosen, false
	}
	digit, ok := MenuKeystrokeFrom(form.Options, chosen)
	if !ok {
		// Names no offered option; UnmatchedMenuReply / AgyAnswerKey refuse it.
		return chosen, false
	}
	chosenLabel := ""
	for _, o := range form.Options {
		if o.Number == digit {
			chosenLabel = o.Label
			break
		}
	}
	if !AgyWideningOption(chosenLabel) {
		return chosen, false
	}
	narrow, n := "", 0
	for _, o := range form.Options {
		if AgyWideningOption(o.Label) || !AgyGrantingOption(o.Label) {
			continue
		}
		narrow, n = o.Label, n+1
	}
	if n != 1 {
		return chosen, false
	}
	return narrow, true
}
