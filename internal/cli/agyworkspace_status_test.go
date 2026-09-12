package cli_test

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/buildinfo"
	"github.com/0xGosu/herdr-auto-pilot/internal/daemonhealth"
)

// The daemon-side test proves the mismatch is RECORDED. This proves it reaches
// an operator, which is a separate failure: a health field that nothing renders
// is exactly the "works but is undiscoverable" gap the shipped-reference tests
// in internal/frontend exist to argue about.
//
// The PID match is load-bearing — the health record is only trusted when it
// belongs to the lock holder, so a record written under a different pid is
// ignored and this test would pass while asserting nothing.
func TestStatusReportsAnAgyWorkspaceMismatch(t *testing.T) {
	app, _ := testApp(t)
	app.DaemonInfo = func() (bool, int, string) { return true, os.Getpid(), buildinfo.Version }
	if err := daemonhealth.Write(app.StateDir, daemonhealth.Health{
		PID: os.Getpid(), HeartbeatAt: time.Now(),
		AgyWorkspace: []daemonhealth.AgyWorkspaceMismatch{{
			AgentID: "pane-agy", Name: "quick-lemur",
			Root: "/workspaces/main-checkout", Elsewhere: "/workspaces/worktree-agent-no23",
		}},
	}); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, app, "status")
	if err != nil {
		t.Fatal(err)
	}
	// The name, both directories, and — the point of the line — the remedy.
	// An operator shown two paths would not know that RESTARTING the agent is
	// what stops the prompts.
	for _, want := range []string{
		"agy workspace:", "quick-lemur",
		"/workspaces/main-checkout", "/workspaces/worktree-agent-no23",
		"restart it in",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status must carry %q, got:\n%s", want, out)
		}
	}
}

// The control: with nothing recorded the line must be absent. Without it, a
// render that printed unconditionally would still pass the case above.
func TestStatusOmitsTheAgyWorkspaceLineWhenThereIsNoMismatch(t *testing.T) {
	app, _ := testApp(t)
	app.DaemonInfo = func() (bool, int, string) { return true, os.Getpid(), buildinfo.Version }
	if err := daemonhealth.Write(app.StateDir, daemonhealth.Health{
		PID: os.Getpid(), HeartbeatAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, app, "status")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "agy workspace:") {
		t.Errorf("a herd with no mismatch must not show the line, got:\n%s", out)
	}
}
