package domain

import "strings"

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
