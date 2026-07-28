package domain

import "strings"

// SuggestedAction extracts the confirmable action from an escalation's
// suggestion: the reply that "confirming this escalation" resolves to.
//
// Shared by the operator-confirm path and the daemon's auto-accept pass — the
// two must agree, or the daemon could auto-accept something other than what
// the operator was shown. Keep in sync with the daemon's suggestionAction.
func SuggestedAction(audit *AuditRecord) string {
	if audit == nil {
		return ""
	}
	sug := audit.Suggestion
	// An idle task suggestion is confirmed into a tasks.md + task source, not
	// sent to the pane as literal text — recognize it before the send-oriented
	// prefixes below.
	if strings.HasPrefix(sug, SuggestTaskPrefix) {
		return SuggestGenerateTask
	}
	sug = StripSourcePrefix(sug)
	for _, p := range []string{"send next declared task: ", "send inferred next task: "} {
		if len(sug) > len(p) && sug[:len(p)] == p {
			if p == "send next declared task: " {
				return ActionNextDeclaredTask
			}
			return ActionNextInferredTask
		}
	}
	// The human-readable "do nothing" suggestion round-trips to the sentinel
	// so a confirmed noop is learned as @noop, never sent as literal text.
	if sug == ActionNoopSuggestion {
		return ActionNoop
	}
	return sug
}

// StripSourcePrefix removes the leading "who suggested this and how" label from
// an escalation suggestion, leaving the action-bearing remainder. The
// task-send prefixes are deliberately NOT here: they can ride behind
// "LLM suggested: ", so both layers must be peeled in order.
func StripSourcePrefix(sug string) string {
	for _, p := range []string{"respond: ", "choose: ", "answer series: ", "on error: ", "LLM suggested: "} {
		if len(sug) > len(p) && sug[:len(p)] == p {
			return sug[len(p):]
		}
	}
	return sug
}

// MaterializeForSend converts symbolic learned actions into the concrete
// suggestion text when the reply is to be sent. It peels the source prefix
// first, exactly as SuggestedAction does: an LLM task review suggests
// "LLM suggested: send next declared task: <text>", and matching the task-send
// prefix against the unpeeled string would miss, returning the raw
// "@next_task:declared" sentinel — which the caller would then type into the
// pane.
func MaterializeForSend(action string, audit *AuditRecord) string {
	if audit != nil && (action == ActionNextDeclaredTask || action == ActionNextInferredTask) {
		sug := StripSourcePrefix(audit.Suggestion)
		for _, p := range []string{"send next declared task: ", "send inferred next task: "} {
			if len(sug) > len(p) && sug[:len(p)] == p {
				return sug[len(p):]
			}
		}
	}
	return action
}
