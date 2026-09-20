package domain

import "strings"

// OperatorTyping reports whether a human has a half-written message sitting in
// the agent's composer right now.
//
// Twice in one day (#526) an idle escalation fired while an operator's draft sat
// in the input box; an automatic send would have appended hap's text to theirs
// and submitted the lot. `claudeComposerEmpty` already states the asymmetry that
// makes this worth asking: a false "not empty" costs one deferred attempt,
// retried on the next capture, while a false "empty" types over somebody's work.
//
// known=false is UNKNOWN and is the common case, not a failure: the daemon's
// classification read is `--source recent`, a CONSUMING delta that routinely
// returns no footer at all. A caller must treat it as "carry on as before" — a
// guard that suppressed on unknown would suppress on nearly every event and
// switch its feature off. Callers wanting certainty read `--source visible`
// first (daemon.readVisible), which is what the session-rename push does.
//
// Gated on agent type, and codex deliberately answers UNKNOWN — see
// codexOperatorTyping.
func OperatorTyping(agentType, pane string) (typing, known bool) {
	switch modeAgentKind(agentType) {
	case "claude":
		sess, ok := ClaudeSessionFromPane(pane)
		if !ok {
			return false, false
		}
		return !sess.ComposerEmpty, true
	case AgentTypeAgy:
		return agyOperatorTyping(pane)
	case "codex":
		return codexOperatorTyping(pane)
	default:
		return false, false
	}
}

// agyOperatorTyping reads the caret line of a POSITIVELY located agy composer.
//
// AgyComposerReady cannot answer this on its own: it folds emptiness together
// with "no modal, no survey, no working turn", so its false covers four states
// and only one of them is a draft. AgyComposerVisible separates them — it stays
// true while the operator holds a draft, which is exactly what its own comment
// says it exists for — so the caret line is read only once the sandwich is
// proven, and everything else is UNKNOWN.
func agyOperatorTyping(pane string) (typing, known bool) {
	if !AgyComposerVisible(pane) {
		return false, false
	}
	lines := agyDropBackgroundStrip(
		trimTrailingBlank(strings.Split(strings.ReplaceAll(pane, "\r", ""), "\n")))
	caret := strings.TrimSpace(lines[len(lines)-3])
	// The same two spellings AgyComposerReady accepts as empty: a bare caret,
	// and the mode placeholder agy paints outside its default mode. The
	// placeholder is NOT a draft — nothing has been typed.
	if caret == ">" || agyModePlaceholderRE.MatchString(caret) {
		return false, true
	}
	return true, true
}

// codexOperatorTyping always answers UNKNOWN, and that is a deliberate refusal
// rather than a gap waiting to be filled carelessly.
//
// The shape is reachable — codexComposerBeforeFooterRE already locates the "›"
// line, it simply matches it outside its capture group. What is missing is the
// evidence to READ it: codex renders suggestion text on the caret line of an
// EMPTY composer (`internal/classify/testdata/transcripts/codex_idle.txt` ends
// "› Summarize recent commits" on a parked agent), and this package has no live
// sample proving which strings are placeholders and which are drafts. Guessing
// either way is worse than not answering: reading the placeholder as a draft
// withholds every hand-out from every parked codex agent, silently, and reading
// a draft as a placeholder is the send this predicate exists to prevent.
//
// To finish it: capture one codex pane with an empty composer and one with a
// real draft, and key on whatever separates them.
func codexOperatorTyping(string) (typing, known bool) { return false, false }
