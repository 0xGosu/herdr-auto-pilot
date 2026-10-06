package cli_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

// seedNode opens the test database as another node and makes it a machine
// last heard at `at`, with one named agent and one pending escalation.
func seedNode(t *testing.T, app *frontend.App, id, label string, at time.Time) *store.Store {
	t.Helper()
	ctx := context.Background()
	other, err := store.OpenAs(filepath.Join(filepath.Dir(app.ConfigPath), "t.db"), id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Close() })
	if err := other.UpsertNode(ctx, domain.NodeInfo{Label: label, LastSeen: at}); err != nil {
		t.Fatal(err)
	}
	if err := other.PublishRoster(ctx, []domain.RosterAgent{
		{AgentID: "1", PaneID: "1", AgentType: "claude", Status: "idle", TerminalID: "t1", SeenAt: at}}, at); err != nil {
		t.Fatal(err)
	}
	if err := other.AssignAgentName(ctx, "1", label+"-worker"); err != nil {
		t.Fatal(err)
	}
	if _, err := other.AppendAudit(ctx, domain.AuditRecord{Signature: "idle:" + id,
		Trigger: "agent idle", SituationType: domain.SituationIdle, AgentID: "1",
		Action: "escalated", Rationale: "[no_task_source]", Status: "escalated", CreatedAt: at}); err != nil {
		t.Fatal(err)
	}
	return other
}

// TestNodesListsEveryOtherNodeWithItsLiveness: the listing names each other
// machine with a live/stale/offline word and when it last reported, oldest
// first, and never lists the node it runs on.
func TestNodesListsEveryOtherNodeWithItsLiveness(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	now := time.Now()
	if err := st.UpsertNode(ctx, domain.NodeInfo{Label: "here", LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	seedNode(t, app, "aaaaaaaaaaaaaaaa", "gone-box", now.Add(-20*24*time.Hour))
	seedNode(t, app, "cccccccccccccccc", "napping", now.Add(-10*time.Minute))
	seedNode(t, app, "dddddddddddddddd", "busy", now)

	out, err := run(t, app, "nodes")
	if err != nil {
		t.Fatal(err)
	}
	lines := tabbedLines(out)
	if len(lines) != 3 {
		t.Fatalf("rows = %d, want the three other nodes and not this one:\n%s", len(lines), out)
	}
	for i, want := range [][2]string{{"gone-box", "offline"}, {"napping", "stale"}, {"busy", "live"}} {
		if lines[i][0] != want[0] || lines[i][3] != want[1] {
			t.Errorf("row %d = %q, want %s %s (oldest first)", i, lines[i], want[0], want[1])
		}
	}
	if !strings.Contains(lines[0][5], "20d ago") {
		t.Errorf("offline row lacks its age: %q", lines[0])
	}
}

// TestNodesPruneWithoutYesChangesNothing: the bare prune is a listing — the
// rows are deleted from the shared database, so the destructive half is opt-in.
func TestNodesPruneWithoutYesChangesNothing(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	now := time.Now()
	seedNode(t, app, "aaaaaaaaaaaaaaaa", "gone-box", now.Add(-20*24*time.Hour))
	seedNode(t, app, "cccccccccccccccc", "recent", now.Add(-2*24*time.Hour))

	for _, args := range [][]string{{"prune"}, {"prune", "--yes", "--dry-run"}} {
		out, err := run(t, app, "nodes", args...)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "gone-box") || strings.Contains(out, "recent") {
			t.Errorf("%v: want only the node past the default 7d listed:\n%s", args, out)
		}
		if !strings.Contains(out, "Nothing was changed") {
			t.Errorf("%v: must say nothing changed:\n%s", args, out)
		}
	}
	nodes, err := st.ListNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 {
		t.Errorf("a listing deleted node rows: %d left", len(nodes))
	}
}

// TestNodesPruneYesRemovesOnlyTheOfflineNodes: --yes prunes the nodes past the
// age — roster, names, node row, pending escalations — and leaves a node that
// reported inside it alone, its queue included.
func TestNodesPruneYesRemovesOnlyTheOfflineNodes(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	now := time.Now()
	seedNode(t, app, "aaaaaaaaaaaaaaaa", "gone-box", now.Add(-20*24*time.Hour))
	seedNode(t, app, "cccccccccccccccc", "recent", now.Add(-2*24*time.Hour))

	out, err := run(t, app, "nodes", "prune", "--yes")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "pruned gone-box") || !strings.Contains(out, "1 escalation(s) dismissed") {
		t.Errorf("output lacks the prune report:\n%s", out)
	}
	roster, published, err := st.FleetRoster(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range roster {
		if r.NodeID == "aaaaaaaaaaaaaaaa" {
			t.Errorf("the pruned node's roster row survived: %+v", r)
		}
	}
	if _, ok := published["aaaaaaaaaaaaaaaa"]; ok {
		t.Error("the pruned node's publish stamp survived")
	}
	if _, ok := published["cccccccccccccccc"]; !ok {
		t.Error("the recent node's roster was pruned too")
	}
	names, err := st.FleetAgentNames(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := names[domain.NodeAgent{NodeID: "aaaaaaaaaaaaaaaa", AgentID: "1"}]; ok {
		t.Error("the pruned node's agent name survived")
	}
	esc, err := st.PendingEscalations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(esc) != 1 || esc[0].NodeID != "cccccccccccccccc" {
		t.Errorf("pending = %+v, want only the recent node's escalation left", esc)
	}
	// A second run finds nothing to do.
	out, err = run(t, app, "nodes", "prune", "--yes")
	if err != nil || !strings.Contains(out, "no node has been offline longer than 7d") {
		t.Errorf("second prune = %v\n%s", err, out)
	}
}

// TestNodesPruneNodeFlagRefusesALiveNodeAndSelf: naming a node that reported
// inside the age, or this machine, is an error — never a silent no-op.
func TestNodesPruneNodeFlagRefusesALiveNodeAndSelf(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	now := time.Now()
	if err := st.UpsertNode(ctx, domain.NodeInfo{Label: "here", LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	seedNode(t, app, "aaaaaaaaaaaaaaaa", "gone-box", now.Add(-20*24*time.Hour))
	seedNode(t, app, "cccccccccccccccc", "recent", now.Add(-2*24*time.Hour))

	if _, err := run(t, app, "nodes", "prune", "--node", "recent", "--yes"); err == nil ||
		!strings.Contains(err.Error(), "not offline long enough") {
		t.Errorf("--node recent err = %v, want a refusal", err)
	}
	if _, err := run(t, app, "nodes", "prune", "--node", "here", "--yes"); !errors.Is(err, domain.ErrNodePruneSelf) {
		t.Errorf("--node here err = %v, want ErrNodePruneSelf", err)
	}
	// A shorter age makes the recent node eligible, and --node keeps the
	// prune to it alone.
	out, err := run(t, app, "nodes", "prune", "--node", "recent", "--older-than", "36h", "--yes")
	if err != nil || !strings.Contains(out, "pruned recent") || strings.Contains(out, "gone-box") {
		t.Errorf("--node recent --older-than 36h = %v\n%s", err, out)
	}
	for _, bad := range []string{"0d", "-3h", "week", "7"} {
		if _, err := run(t, app, "nodes", "prune", "--older-than", bad); err == nil {
			t.Errorf("--older-than %q accepted", bad)
		}
	}
}

// tabbedLines splits the tab-separated rows out of a command's output.
func tabbedLines(out string) [][]string {
	var rows [][]string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "\t") && !strings.HasPrefix(line, "  ") {
			rows = append(rows, strings.Split(line, "\t"))
		}
	}
	return rows
}
