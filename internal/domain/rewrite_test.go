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
			name:     "empty template uses default passthrough",
			template: "",
			original: "go test ./...",
			want:     "go test ./...",
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
			want:     "go vet ./...",
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
			want:     "",
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

func TestIsRewriteNoChange(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"@rewrite:nochange", true},
		{"@Rewrite:NoChange", true},
		{"  @rewrite:nochange \n", true},
		{"nochange", false},
		{"@rewrite:nochange please", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := IsRewriteNoChange(tt.in); got != tt.want {
			t.Errorf("IsRewriteNoChange(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestIsRewriteNoop(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"@noop", true},
		{"@NoOp", true},
		{" @noop \n", true},
		// Bare spellings are free text a rewrite could legitimately
		// produce — only the @ prefix marks sentinel intent.
		{"noop", false},
		{"no_op", false},
		{"no-op", false},
		{"do @noop now", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := IsRewriteNoop(tt.in); got != tt.want {
			t.Errorf("IsRewriteNoop(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
