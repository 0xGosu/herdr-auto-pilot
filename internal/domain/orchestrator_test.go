package domain

import (
	"strings"
	"testing"
)

func TestOrchestratorLaunch(t *testing.T) {
	kind, args, err := OrchestratorLaunch([]string{"/usr/local/bin/claude", "--model", "opus"})
	if err != nil || kind != "claude" || strings.Join(args, " ") != "--model opus" {
		t.Fatalf("OrchestratorLaunch = %q %q %v", kind, args, err)
	}
	for _, argv := range [][]string{nil, {"codex"}, {"my-claude-wrapper", "--x"}} {
		if _, _, err := OrchestratorLaunch(argv); err == nil {
			t.Errorf("OrchestratorLaunch(%q) accepted a command herdr cannot start as claude", argv)
		}
	}
}

func TestOrchestratorIdentityMatching(t *testing.T) {
	id := OrchestratorIdentity{PaneID: "w9:p1", TerminalID: "term_a"}
	cases := []struct {
		name          string
		tr            AgentTransition
		match, recycl bool
	}{
		{"same pane and terminal", AgentTransition{PaneID: "w9:p1", TerminalID: "term_a"}, true, false},
		{"event with no terminal id", AgentTransition{PaneID: "w9:p1"}, true, false},
		{"agent id only", AgentTransition{AgentID: "w9:p1"}, true, false},
		{"recycled pane", AgentTransition{PaneID: "w9:p1", TerminalID: "term_b"}, false, true},
		{"another pane", AgentTransition{PaneID: "w9:p2", TerminalID: "term_a"}, false, false},
	}
	for _, tc := range cases {
		if got := id.Matches(tc.tr); got != tc.match {
			t.Errorf("%s: Matches = %v, want %v", tc.name, got, tc.match)
		}
		if got := id.RecycledBy(tc.tr); got != tc.recycl {
			t.Errorf("%s: RecycledBy = %v, want %v", tc.name, got, tc.recycl)
		}
	}
	if (OrchestratorIdentity{}).Matches(AgentTransition{PaneID: ""}) {
		t.Error("an unknown identity matched an agent")
	}
}

// Only a session hap briefed itself gets the dormancy messages. An adopted one
// is recorded Briefed with no attempts; one whose brief never landed is not
// Briefed at all.
func TestOrchestratorIdentityHapBriefed(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   OrchestratorIdentity
		want bool
	}{
		{"briefed by hap", OrchestratorIdentity{PaneID: "p", Briefed: true, BriefAttempts: 1}, true},
		{"briefed after a retry", OrchestratorIdentity{PaneID: "p", Briefed: true, BriefAttempts: 2}, true},
		{"adopted", OrchestratorIdentity{PaneID: "p", Briefed: true}, false},
		{"brief never landed", OrchestratorIdentity{PaneID: "p", BriefAttempts: 3}, false},
		{"fresh", OrchestratorIdentity{PaneID: "p"}, false},
	} {
		if got := tc.id.HapBriefed(); got != tc.want {
			t.Errorf("%s: HapBriefed() = %v, want %v", tc.name, got, tc.want)
		}
	}
}
