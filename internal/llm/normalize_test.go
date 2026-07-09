package llm

import (
	"fmt"
	"testing"
)

func TestNormalizeLLMCommand(t *testing.T) {
	mcpJSON := `{"mcpServers":{"hap":{"command":"/x/hap","args":["mcp"]}}}`

	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "claude prompt after flags is moved next to -p (user-reported)",
			in: []string{"claude", "-p",
				"--mcp-config", mcpJSON,
				"--allowedTools", "mcp__hap__get_context,mcp__hap__submit_decision",
				"Use the hap MCP tools."},
			want: []string{"claude", "-p", "Use the hap MCP tools.",
				"--mcp-config", mcpJSON,
				"--allowedTools", "mcp__hap__get_context,mcp__hap__submit_decision"},
		},
		{
			name: "claude already correct is untouched",
			in:   []string{"claude", "-p", "do it", "--mcp-config", mcpJSON},
			want: []string{"claude", "-p", "do it", "--mcp-config", mcpJSON},
		},
		{
			name: "claude unknown flag bails out unchanged",
			in:   []string{"claude", "-p", "--mystery-flag", "value", "prompt"},
			want: []string{"claude", "-p", "--mystery-flag", "value", "prompt"},
		},
		{
			name: "claude bool flags are classified",
			in:   []string{"claude", "-p", "--verbose", "--mcp-config", mcpJSON, "prompt here"},
			want: []string{"claude", "-p", "prompt here", "--verbose", "--mcp-config", mcpJSON},
		},
		{
			name: "claude no positional stays put",
			in:   []string{"claude", "-p", "--verbose"},
			want: []string{"claude", "-p", "--verbose"},
		},
		{
			name: "agy prompt after flags is moved next to --print",
			in:   []string{"agy", "--print", "--dangerously-skip-permissions", "--mode", "plan", "do the thing"},
			want: []string{"agy", "--print", "do the thing", "--dangerously-skip-permissions", "--mode", "plan"},
		},
		{
			name: "agy already correct is untouched",
			in:   []string{"agy", "--print", "do the thing", "--dangerously-skip-permissions"},
			want: []string{"agy", "--print", "do the thing", "--dangerously-skip-permissions"},
		},
		{
			name: "codex without exec gets it inserted",
			in:   []string{"codex", "--skip-git-repo-check", "-c", "mcp_servers.hap.command=/x/hap", "do the thing"},
			want: []string{"codex", "exec", "--skip-git-repo-check", "-c", "mcp_servers.hap.command=/x/hap", "do the thing"},
		},
		{
			name: "codex with exec is untouched",
			in:   []string{"codex", "exec", "do the thing"},
			want: []string{"codex", "exec", "do the thing"},
		},
		{
			name: "codex mcp subcommand is untouched",
			in:   []string{"codex", "mcp", "serve"},
			want: []string{"codex", "mcp", "serve"},
		},
		{
			name: "unknown CLI is untouched",
			in:   []string{"my-llm", "--flag", "prompt"},
			want: []string{"my-llm", "--flag", "prompt"},
		},
		{
			name: "absolute path to claude is recognized",
			in:   []string{"/usr/local/bin/claude", "-p", "--verbose", "prompt"},
			want: []string{"/usr/local/bin/claude", "-p", "prompt", "--verbose"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := NormalizeLLMCommand(c.in)
			if fmt.Sprint(got) != fmt.Sprint(c.want) {
				t.Errorf("NormalizeLLMCommand(%q)\n got %q\nwant %q", c.in, got, c.want)
			}
		})
	}
}
