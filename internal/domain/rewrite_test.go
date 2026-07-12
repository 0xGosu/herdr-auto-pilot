package domain

import "testing"

func TestApplyRewriteFallback(t *testing.T) {
	tests := []struct {
		name      string
		template  string
		original  string
		agentName string
		want      string
	}{
		{
			name:     "empty template uses default",
			template: "",
			original: "go test ./...",
			want:     "You must act based on the following: go test ./...",
		},
		{
			name:     "custom template",
			template: "Please do this now: {original_text}",
			original: "retry the build",
			want:     "Please do this now: retry the build",
		},
		{
			name:     "template without placeholder falls back to default",
			template: "just some words",
			original: "go vet ./...",
			want:     "You must act based on the following: go vet ./...",
		},
		{
			name:     "placeholder in original not re-expanded",
			template: "Do: {original_text}",
			original: "echo {original_text}",
			want:     "Do: echo {original_text}",
		},
		{
			name:     "empty original still renders",
			template: "",
			original: "",
			want:     "You must act based on the following: ",
		},
		{
			name:      "agent_name substituted",
			template:  "{agent_name}, do: {original_text}",
			original:  "run tests",
			agentName: "brave-otter",
			want:      "brave-otter, do: run tests",
		},
		{
			name:      "agent_name in original not re-expanded",
			template:  "{agent_name}: {original_text}",
			original:  "print {agent_name}",
			agentName: "calm-lynx",
			want:      "calm-lynx: print {agent_name}",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ApplyRewriteFallback(tt.template, tt.original, tt.agentName); got != tt.want {
				t.Errorf("ApplyRewriteFallback(%q, %q, %q) = %q, want %q",
					tt.template, tt.original, tt.agentName, got, tt.want)
			}
		})
	}
}
