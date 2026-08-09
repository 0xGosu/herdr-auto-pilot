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
			name: "repeated --mcp-config still yields one flag",
			argv: []string{"claude", "--mcp-config", "a.json", "--mcp-config", "b.json"},
			want: []string{"claude", "--mcp-config", "a.json", "--mcp-config", "b.json", "--strict-mcp-config"},
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

// TestStrictMCPConfigSurvivesPromptRepair pins that the appended flag does not
// break claude's prompt-adjacency repair. The template here is deliberately
// MISORDERED (the prompt sits after --model), so NormalizeLLMCommand actually
// walks the flags instead of taking its already-adjacent early return — which
// is what makes the case able to fail: an unclassifiable trailing flag makes
// fixPromptAdjacency bail out and leaves the prompt stranded, and claude then
// dies with "Input must be provided either through stdin or as a prompt
// argument". The classification lives in claudeBoolFlags.
func TestStrictMCPConfigSurvivesPromptRepair(t *testing.T) {
	argv := InjectStrictMCPConfig([]string{"claude", "-p", "--model", "opus", "decide", "--mcp-config", "{}"})
	argv = NormalizeLLMCommand(argv)
	i := slices.Index(argv, "-p")
	if i < 0 || i+1 >= len(argv) || argv[i+1] != "decide" {
		t.Errorf("prompt must be repaired to sit immediately after -p, got %q", argv)
	}
	if !slices.Contains(argv, StrictMCPConfigFlag) {
		t.Errorf("the flag must survive normalization, got %q", argv)
	}
}

// TestInjectionRunsOnTheTemplateNotTheSubstitutedArgv pins the real reason the
// injection sits before placeholder substitution: detection then only ever sees
// OPERATOR-authored argv. A prompt carrying untrusted pane text that happens to
// expand to exactly "--mcp-config" must not be able to make hap declare an MCP
// set the operator never wrote.
func TestInjectionRunsOnTheTemplateNotTheSubstitutedArgv(t *testing.T) {
	tmpl := []string{"claude", "-p", "{pane_excerpt}"}
	if got := InjectStrictMCPConfig(tmpl); slices.Contains(got, StrictMCPConfigFlag) {
		t.Errorf("a template with no --mcp-config must be left alone, got %q", got)
	}
	// The same argv AFTER a hostile substitution — proof the ordering is what
	// keeps this out of reach, not the detection itself.
	substituted := []string{"claude", "-p", "--mcp-config"}
	if got := InjectStrictMCPConfig(substituted); !slices.Contains(got, StrictMCPConfigFlag) {
		t.Logf("note: post-substitution argv would match (%q) — which is exactly why injection runs on the template", got)
	}
}

// consultArgv runs one real consult through a fake CLI named "claude" (so the
// claude-only gate applies) and returns the argv it actually received, one
// element per entry — compared exactly, since a substring match would also
// accept the flag glued into another element.
func consultArgv(t *testing.T, reqID string, tmpl ...string) []string {
	t.Helper()
	st, db := testStore(t)
	out := filepath.Join(t.TempDir(), "argv.txt")
	script := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > '"+out+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	a := &Adapter{
		CommandTemplate: append([]string{script}, tmpl...),
		Timeout:         5 * time.Second,
		DBPath:          db, Store: st, SelfPath: "/bin/true",
	}
	req := domain.LLMRequest{RequestID: reqID, CreatedAt: time.Now()}
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
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// TestConsultPassesStrictMCPConfig is the end-to-end check that a real consult
// spawn carries the flag: it is what stops the agent's own project .mcp.json
// from adding servers to hap's decision run.
func TestConsultPassesStrictMCPConfig(t *testing.T) {
	argv := consultArgv(t, "req-strict", "-p", "decide", "--mcp-config", "{}")
	if !slices.Contains(argv, StrictMCPConfigFlag) {
		t.Errorf("the consult argv must carry %s, got %q", StrictMCPConfigFlag, argv)
	}
}

// TestConsultWithoutMCPConfigIsNotMadeStrict is the bound the whole change
// rests on: a template that names no MCP set of its own is asserting nothing,
// so silently switching off the operator's user-level servers there would be a
// capability removal they never asked for.
func TestConsultWithoutMCPConfigIsNotMadeStrict(t *testing.T) {
	argv := consultArgv(t, "req-nostrict", "-p", "decide")
	if slices.Contains(argv, StrictMCPConfigFlag) {
		t.Errorf("a template with no --mcp-config must not be made strict, got %q", argv)
	}
}
