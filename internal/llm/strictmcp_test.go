package llm

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

func TestInjectStrictMCPConfig(t *testing.T) {
	tests := []struct {
		name string
		argv []string
		want []string
	}{
		{
			name: "claude with --mcp-config gets the flag",
			argv: []string{"claude", "-p", "decide", "--mcp-config", "{}"},
			want: []string{"claude", "-p", "decide", "--mcp-config", "{}", "--strict-mcp-config"},
		},
		{
			name: "the =form counts as passing --mcp-config",
			argv: []string{"claude", "--mcp-config={}", "-p", "decide"},
			want: []string{"claude", "--mcp-config={}", "-p", "decide", "--strict-mcp-config"},
		},
		{
			name: "an absolute path still resolves to claude",
			argv: []string{"/usr/local/bin/claude", "--mcp-config", "{}"},
			want: []string{"/usr/local/bin/claude", "--mcp-config", "{}", "--strict-mcp-config"},
		},
		// Without --mcp-config the template asserts no MCP set of its own, so
		// making one exclusive would remove the operator's user-level servers —
		// a capability they never asked to lose. This is the shape the shipped
		// task-generation and learn-from-user recipes have.
		{
			name: "no --mcp-config is left alone",
			argv: []string{"claude", "-p", "suggest a task"},
			want: []string{"claude", "-p", "suggest a task"},
		},
		{
			name: "never doubled",
			argv: []string{"claude", "--mcp-config", "{}", "--strict-mcp-config"},
			want: []string{"claude", "--mcp-config", "{}", "--strict-mcp-config"},
		},
		// codex and agy have no such flag; appending an unknown one is an argv
		// error that would fail every run.
		{
			name: "codex is untouched",
			argv: []string{"codex", "exec", "-c", "mcp_servers.hap.command=x"},
			want: []string{"codex", "exec", "-c", "mcp_servers.hap.command=x"},
		},
		{
			name: "agy is untouched",
			argv: []string{"agy", "--print", "decide", "--mcp-config", "{}"},
			want: []string{"agy", "--print", "decide", "--mcp-config", "{}"},
		},
		{name: "empty argv", argv: nil, want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := InjectStrictMCPConfig(slices.Clone(tc.argv))
			if !slices.Equal(got, tc.want) {
				t.Errorf("InjectStrictMCPConfig(%q) = %q, want %q", tc.argv, got, tc.want)
			}
		})
	}
}

// TestStrictMCPConfigKeepsThePromptBesideItsFlag pins the ORDER: the flag is
// appended to the template, then NormalizeLLMCommand runs — so claude's prompt
// is moved back beside its -p over both this addition and the session id.
// Appending after the normalizer would strand the prompt and fail every run
// with "Input must be provided either through stdin or as a prompt argument".
func TestStrictMCPConfigKeepsThePromptBesideItsFlag(t *testing.T) {
	argv := InjectStrictMCPConfig([]string{"claude", "-p", "decide", "--mcp-config", "{}"})
	argv = NormalizeLLMCommand(argv)
	i := slices.Index(argv, "-p")
	if i < 0 || i+1 >= len(argv) || argv[i+1] != "decide" {
		t.Errorf("prompt must stay immediately after -p, got %q", argv)
	}
	if !slices.Contains(argv, StrictMCPConfigFlag) {
		t.Errorf("the flag must survive normalization, got %q", argv)
	}
}

// TestConsultPassesStrictMCPConfig is the end-to-end check that a real consult
// spawn carries the flag: the guard is what stops the agent's own project
// .mcp.json from adding servers to hap's decision run.
func TestConsultPassesStrictMCPConfig(t *testing.T) {
	st, db := testStore(t)
	out := filepath.Join(t.TempDir(), "argv.txt")
	// Named "claude" so the claude-only gate applies to this fake.
	dir := t.TempDir()
	script := filepath.Join(dir, "claude")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > '"+out+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	a := &Adapter{
		CommandTemplate: []string{script, "-p", "decide", "--mcp-config", "{}"},
		Timeout:         5 * time.Second,
		DBPath:          db, Store: st, SelfPath: "/bin/true",
	}
	req := domain.LLMRequest{RequestID: "req-strict", CreatedAt: time.Now()}
	ctx := context.Background()
	if _, err := st.StageLLMRequest(ctx, req); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertLLMDecision(ctx, domain.LLMDecision{
		RequestID: req.RequestID, Action: "ok", Status: "pending", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Consult(ctx, req); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), StrictMCPConfigFlag) {
		t.Errorf("the consult argv must carry %s, got:\n%s", StrictMCPConfigFlag, data)
	}
}
