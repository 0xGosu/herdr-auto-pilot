package domain

import "testing"

func TestIsPlaceholderAgentRequiresBothFieldsToBeUnknown(t *testing.T) {
	tests := []struct {
		name      string
		agentType string
		status    string
		want      bool
	}{
		{name: "both empty", want: true},
		{name: "herdr sentinels", agentType: "undefined", status: "unknown", want: true},
		{name: "mixed sentinels", agentType: " UNKNOWN ", status: " Undefined ", want: true},
		{name: "real type unknown status", agentType: "claude", status: "unknown", want: false},
		{name: "unknown type active status", agentType: "undefined", status: "working", want: false},
		{name: "real agent", agentType: "codex", status: "blocked", want: false},
		// Detection events never carry a status (it decodes as ""), so the
		// empty-string form of each field — not just the literal
		// "undefined"/"unknown" sentinels — must be exercised on both sides.
		{name: "real type empty status", agentType: "claude", status: "", want: false},
		{name: "empty type real status", agentType: "", status: "working", want: false},
		{name: "empty type explicit unknown status", agentType: "", status: "unknown", want: true},
		{name: "explicit undefined type empty status", agentType: "undefined", status: "", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsPlaceholderAgent(tt.agentType, tt.status); got != tt.want {
				t.Fatalf("IsPlaceholderAgent(%q, %q) = %v, want %v",
					tt.agentType, tt.status, got, tt.want)
			}
		})
	}
}
