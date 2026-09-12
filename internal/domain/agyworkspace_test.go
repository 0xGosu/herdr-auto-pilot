package domain

import "testing"

// The agy "outside workspace" approval is the only evidence hap has of where an
// agent's work actually is: the agent's own cwd never moves off the directory
// it was started in, so a mismatch is invisible without reading the prompt.

func TestAgyOutsideWorkspacePath(t *testing.T) {
	access := "File access\n────────────────\nRead: /workspaces/other/main.go\n" +
		"Reason: outside workspace\n\nAllow access to this file?\n" +
		"> 1. Yes, allow access\n  2. No, deny access\n\n  ↑/↓ Navigate\n"
	// Same form, a reason that is NOT about the workspace.
	otherReason := "File access\n────────────────\nRead: /workspaces/other/main.go\n" +
		"Reason: protected path\n\nAllow access to this file?\n" +
		"> 1. Yes, allow access\n  2. No, deny access\n\n  ↑/↓ Navigate\n"
	// A shell approval carries no file target at all.
	shell := "Run this command?\n> 1. Yes, run command\n  2. No, cancel\n\n  ↑/↓ Navigate\n"

	cases := []struct {
		name     string
		pane     string
		wantPath string
		wantOK   bool
	}{
		{"an outside-workspace read", access, "/workspaces/other/main.go", true},
		{"a different reason is not this signal", otherReason, "", false},
		{"a shell approval has no path", shell, "", false},
		{"an empty pane", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := AgyOutsideWorkspacePath(tc.pane)
			if ok != tc.wantOK || got != tc.wantPath {
				t.Errorf("AgyOutsideWorkspacePath = (%q, %v), want (%q, %v)",
					got, ok, tc.wantPath, tc.wantOK)
			}
		})
	}
}

func TestAgyWorkspaceMismatch(t *testing.T) {
	cases := []struct {
		name string
		root string
		path string
		want bool
	}{
		{"work outside the root", "/workspaces/repo", "/workspaces/other/main.go", true},
		{"work inside the root", "/workspaces/repo", "/workspaces/repo/internal/x.go", false},
		{"the root itself", "/workspaces/repo", "/workspaces/repo", false},
		// A plain prefix test would call this a match: /workspaces/repo-two is
		// a different directory that merely starts with the same bytes.
		{"a sibling sharing a prefix is outside", "/workspaces/repo", "/workspaces/repo-two/x.go", true},
		// Unknown is never evidence: paneCwd returns "" for a cold pane, and
		// rendering that as a misconfiguration would flag every fresh agent.
		{"an unread root is not a mismatch", "", "/workspaces/other/x.go", false},
		{"no path is not a mismatch", "/workspaces/repo", "", false},
		// herdr renders a removed directory with this suffix; without
		// stripping it, such an agent mismatches against itself.
		{"a deleted root still compares", "/workspaces/repo (deleted)", "/workspaces/repo/x.go", false},
		{"an agent started at / contains everything", "/", "/anywhere/x.go", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AgyWorkspaceMismatch(tc.root, tc.path); got != tc.want {
				t.Errorf("AgyWorkspaceMismatch(%q, %q) = %v, want %v",
					tc.root, tc.path, got, tc.want)
			}
		})
	}
}

// The reported directory must be stable across runs: naming whichever file was
// read first tells the operator nothing about where to restart the agent.
func TestAgyWorkspaceRootOf(t *testing.T) {
	cases := []struct{ path, want string }{
		{"/workspaces/worktree-agent-no23/internal/daemon/x.go", "/workspaces/worktree-agent-no23"},
		{"/workspaces/other", "/workspaces/other"},
		{"/tmp/x.go", "/tmp/x.go"},
	}
	for _, tc := range cases {
		if got := AgyWorkspaceRootOf(tc.path); got != tc.want {
			t.Errorf("AgyWorkspaceRootOf(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}
