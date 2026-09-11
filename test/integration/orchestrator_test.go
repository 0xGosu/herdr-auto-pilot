//go:build integration

package integration

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/herdr"
)

// closeWorkspacesLabelled closes every workspace carrying label — the cleanup
// for a test that let the launcher create one.
func closeWorkspacesLabelled(t *testing.T, cli *herdr.CLI, label string) {
	t.Helper()
	ws, err := cli.ListWorkspaces(context.Background())
	if err != nil {
		t.Logf("cleanup: listing workspaces failed: %v", err)
		return
	}
	for _, w := range ws {
		if w.Label == label {
			tryHerdr("workspace", "close", w.ID)
		}
	}
}

// TestRealAgentLauncherShapes pins the herdr verbs the full-self-prompting
// orchestrator depends on — `agent get` for a missing agent, `workspace create`
// and `tab create` into a workspace found again by its label — against a real
// herdr. No agent is started, so it spends nothing; the unit suite fakes all
// three, so only this catches herdr reshaping their output.
func TestRealAgentLauncherShapes(t *testing.T) {
	requireHerdr(t)
	cli := herdr.NewCLI()
	ctx := context.Background()

	if _, found, err := cli.AgentByName(ctx, "hap-itest-no-such-agent"); err != nil || found {
		t.Fatalf("AgentByName(missing) = found %v, err %v; want not found and no error "+
			"(an error here would make the daemon back off instead of creating the session)", found, err)
	}

	label := "hap-itest-orch-" + strconv.FormatInt(time.Now().UnixNano()%1_000_000, 36)
	t.Cleanup(func() { closeWorkspacesLabelled(t, cli, label) })
	cwd := t.TempDir()
	first, err := cli.NewPaneInWorkspace(ctx, label, "orchestrator", cwd)
	if err != nil {
		t.Fatalf("creating the labelled workspace: %v", err)
	}
	second, err := cli.NewPaneInWorkspace(ctx, label, "orchestrator", cwd)
	if err != nil {
		t.Fatalf("opening a tab in the labelled workspace: %v", err)
	}
	if first == "" || first == second {
		t.Fatalf("pane ids %q and %q: want two distinct panes", first, second)
	}
	a, err := cli.PaneInfo(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := cli.PaneInfo(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	if a.WorkspaceID == "" || a.WorkspaceID != b.WorkspaceID {
		t.Fatalf("workspaces %q and %q: the labelled workspace was not reused", a.WorkspaceID, b.WorkspaceID)
	}
	ws, err := cli.ListWorkspaces(ctx)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, w := range ws {
		if w.Label == label {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d workspaces carry the label %q, want exactly one", n, label)
	}
}

// TestRealOrchestratorAgentStart starts a real claude through the launcher and
// finds it again by name — the round trip the daemon relies on to record the
// session's pane and terminal. Nothing is prompted, so a started claude spends
// nothing; gated on HAP_ITEST_CLAUDE all the same, like every case that runs
// the agent.
func TestRealOrchestratorAgentStart(t *testing.T) {
	requireHerdr(t)
	if os.Getenv("HAP_ITEST_CLAUDE") != "1" {
		t.Skip("set HAP_ITEST_CLAUDE=1 to start a real claude")
	}
	cli := herdr.NewCLI()
	ctx := context.Background()
	name := sanitizeAgentName("orch-" + t.Name())
	// /tmp is pre-trusted, so no first-run prompt stands between start and
	// readiness.
	pane := newScratchPane(t, "/tmp", "hap-itest-orch")
	if err := cli.StartAgent(ctx, name, "claude", pane, nil); err != nil {
		t.Skipf("could not start claude in pane %s: %v", pane, err)
	}
	a, found, err := cli.AgentByName(ctx, name)
	if err != nil || !found {
		t.Fatalf("AgentByName(%q) = found %v, err %v after a successful start", name, found, err)
	}
	if a.PaneID != pane || a.TerminalID == "" || a.AgentType != "claude" {
		t.Fatalf("AgentByName = %+v; want pane %s, a terminal id and kind claude", a, pane)
	}
}
