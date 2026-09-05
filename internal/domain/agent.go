package domain

import "strings"

// AgentStatusDetected is the status herdr's discovery event carries, and it is
// not a report about what the agent is DOING.
//
// The adapter synthesizes it for pane.agent_detected, which herdr replays for
// every existing pane on every subscribe. Named here because two packages have
// to agree on it: the adapter writes it, and a reader deciding whether an agent
// is available has to know it means "this pane exists", not "this pane is
// busy" — even though AgentBusy, which cannot tell, answers true.
const AgentStatusDetected = "detected"

// IsPlaceholderAgent reports whether Herdr returned an agent-list/event row
// with no usable agent identity or status. Both fields must be placeholders:
// a real agent whose status is temporarily unknown, or a transitioning row
// whose type has not arrived yet, must remain visible.
func IsPlaceholderAgent(agentType, status string) bool {
	return isPlaceholderAgentField(agentType) && isPlaceholderAgentField(status)
}

func isPlaceholderAgentField(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "undefined", "unknown":
		return true
	default:
		return false
	}
}

// KnownAgentType reports whether an agent type names an actual agent, as
// opposed to a placeholder hap or herdr wrote because none was available.
//
// The placeholders are values, not absences: the daemon stores the LITERAL
// "unknown" on a situation herdr reported no type for, and that string travels
// onto decisions, signature state and audit rows — so a caller comparing two
// agent types must not treat `!= ""` as "known". Comparing an "unknown" against
// a real type is not a mismatch; it is an absence of evidence, and code that
// refuses on it refuses the wrong thing.
func KnownAgentType(agentType string) bool {
	return !isPlaceholderAgentField(agentType)
}

// SameAgentType reports whether two agent types are known AND equal — the only
// basis on which a caller may conclude "this is a different agent". Either side
// being a placeholder yields false for BOTH SameAgentType and DifferentAgentType,
// which is the point: neither conclusion is available without evidence.
func SameAgentType(a, b string) bool {
	return KnownAgentType(a) && KnownAgentType(b) &&
		strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

// DifferentAgentType reports whether two agent types are known and disagree.
// Use this — never `a != b` — to decide that a pane now hosts a different agent
// than a stored record describes.
func DifferentAgentType(a, b string) bool {
	return KnownAgentType(a) && KnownAgentType(b) && !SameAgentType(a, b)
}
