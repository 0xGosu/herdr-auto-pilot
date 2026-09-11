//go:build integration

package integration

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/herdr"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
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

	// A failed start closes the pane it opened; a wrong shape here would only
	// log a warning, and the tab leak it exists to stop would come back.
	if err := cli.ClosePane(ctx, second); err != nil {
		t.Fatalf("closing pane %s: %v", second, err)
	}
	if info, err := cli.PaneInfo(ctx, second); err == nil {
		t.Fatalf("pane %s still exists after ClosePane: %+v", second, info)
	}
}

// TestRealOrchestratorAgentStart starts a real claude through the launcher and
// finds it again by name — the round trip the daemon relies on to record the
// session's pane and terminal — then checks the two things the brief depends on
// that the unit suite can only fake: the fresh session reads as a proven-empty
// composer, and a multi-line message lands. Spends a few tokens.
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

	// The daemon briefs only once the composer is PROVEN empty. A freshly
	// started claude must read that way within a few seconds, or the brief is
	// deferred forever and the operator gets one notification about it.
	var screen string
	deadline := time.Now().Add(30 * time.Second)
	for {
		screen, err = cli.ReadPaneVisible(ctx, pane, 80)
		if err == nil {
			if s, ok := domain.ClaudeSessionFromPane(screen); ok && s.ComposerEmpty {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("a freshly started claude never showed a proven-empty composer (err %v); last screen:\n%s", err, screen)
		}
		time.Sleep(500 * time.Millisecond)
	}

	// The brief is multi-line, so it takes the paste route (`agent prompt`),
	// which needs the agent interactively ready: it must land as ONE message.
	// Spends a few tokens: claude answers it.
	marker := "hap-itest-brief-" + strconv.FormatInt(time.Now().UnixNano()%1_000_000, 36)
	brief := "This is a test of message delivery; reply with just the word ok.\n" +
		"Marker: " + marker + "\nDo not run any tools."
	if err := ports.SendToAgent(ctx, cli, pane, "claude", brief); err != nil {
		t.Fatalf("sending a multi-line brief: %v", err)
	}
	// Not waitForPaneText: a fresh claude renders its transcript at the TOP of
	// the pane, dozens of blank rows above the composer, so a 20-line read of
	// a tall pane never reaches it.
	deadline = time.Now().Add(paneTextTimeout)
	for {
		screen, err = cli.ReadPaneVisible(ctx, pane, 400)
		if err == nil && strings.Contains(screen, marker) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the multi-line brief never appeared in pane %s (err %v); last screen:\n%s", pane, err, screen)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
