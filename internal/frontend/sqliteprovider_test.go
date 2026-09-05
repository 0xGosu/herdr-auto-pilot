package frontend_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
	"github.com/0xGosu/herdr-auto-pilot/internal/tasklocator"
)

// TestSQLiteProviderKeepsTheListInTheStore drives the REAL registry (no
// TaskStoreFor seam): a source under provider = "sqlite" resolves to
// db://<this node>/<name>, `add` creates the list in the store and every later
// op mutates it there, and the Tasks view groups it like any source. The
// source names its list explicitly — the derived per-agent form is created
// only for an agent hap has seen, exactly as under the gist provider.
func TestSQLiteProviderKeepsTheListInTheStore(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	if err := os.WriteFile(app.ConfigPath, []byte(
		"[task_source_provider]\nprovider = \"sqlite\"\n\n[[task_sources]]\nagent = \"otter\"\npath = \"otter.md\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	items, _, err := app.AddTask("otter", "", "first task")
	if err != nil {
		t.Fatalf("add through the sqlite provider: %v", err)
	}
	if len(items) != 1 || items[0].Text != "first task" {
		t.Fatalf("items = %+v", items)
	}
	l, err := st.ReadTaskList(ctx, st.NodeID(), "otter.md")
	if err != nil {
		t.Fatalf("the list must live in the store: %v", err)
	}
	if !strings.Contains(l.Content, "- [ ] first task") || !strings.HasPrefix(l.Content, "# Tasks for otter") {
		t.Errorf("stored list = %q, want the header and the item", l.Content)
	}
	if l.AgentName != "otter" {
		t.Errorf("AgentName = %q", l.AgentName)
	}

	if _, err := app.SetTaskDone("otter", "", 1, true); err != nil {
		t.Fatal(err)
	}
	if l, _ = st.ReadTaskList(ctx, st.NodeID(), "otter.md"); !strings.Contains(l.Content, "- [x] first task") {
		t.Errorf("done must land in the store, got %q", l.Content)
	}

	cfg, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	groups := app.TaskGroups(cfg, frontend.Status{})
	if len(groups) != 1 {
		t.Fatalf("groups = %+v", groups)
	}
	want := tasklocator.DBLocator(st.NodeID(), "otter.md")
	if groups[0].Locator != want || groups[0].Err != "" || len(groups[0].Items) != 1 || groups[0].Fleet() {
		t.Errorf("group = %+v, want locator %s with one item and no node", groups[0], want)
	}
	// This node's own lists are NOT fleet groups — they are already shown as
	// configured sources.
	if fleet := app.FleetTaskGroups(ctx, frontend.Status{}); len(fleet) != 0 {
		t.Errorf("own lists must not appear as fleet groups: %+v", fleet)
	}
}

// TestFleetTaskGroupsShowAndEditAnotherNodesList: a list another node keeps in
// the shared store appears as a fleet group labelled with that node, `hap task
// --node` resolves it by agent, and an edit from here lands on that list.
func TestFleetTaskGroupsShowAndEditAnotherNodesList(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	// The other machine: a second node on the same store, heartbeating under
	// its own id and keeping one list.
	const other = "b1b1b1b1b1b1b1b1"
	remote, err := store.OpenAs(filepath.Join(filepath.Dir(app.ConfigPath), "t.db"), other)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { remote.Close() })
	if err := remote.UpsertNode(ctx, domain.NodeInfo{ID: other, Label: "laptop", LastSeen: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := remote.EnsureTaskList(ctx, other, "badger.md", "badger", "# Tasks for badger\n\n- [ ] remote one\n- [ ] remote two\n", time.Now()); err != nil {
		t.Fatal(err)
	}

	status, err := app.GetStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fleet := app.FleetTaskGroups(ctx, status)
	if len(fleet) != 1 {
		t.Fatalf("fleet groups = %+v, want the other node's list", fleet)
	}
	g := fleet[0]
	if g.NodeID != other || g.NodeLabel != "laptop" || g.Source.Agent != "badger" || g.Index != -1 || !g.Fleet() {
		t.Errorf("group = %+v", g)
	}
	if g.Locator != tasklocator.DBLocator(other, "badger.md") || len(g.Items) != 2 {
		t.Errorf("group locator/items = %s / %d", g.Locator, len(g.Items))
	}

	for _, ref := range []string{"badger", "badger.md"} {
		loc, err := app.NodeTaskList(ctx, "laptop", ref)
		if err != nil || loc != g.Locator {
			t.Errorf("NodeTaskList(laptop, %q) = %q, %v", ref, loc, err)
		}
	}
	if _, err := app.NodeTaskList(ctx, "laptop", "nobody"); err == nil || !strings.Contains(err.Error(), "badger.md") {
		t.Errorf("a miss must list the node's lists, got %v", err)
	}
	if _, err := app.NodeTaskList(ctx, "desktop", "badger"); err == nil {
		t.Error("an unknown node must be refused")
	}

	// Edit through the locator, exactly as the CLI and the Tasks tab do.
	if _, err := app.SetTaskDone("", g.Locator, 2, true); err != nil {
		t.Fatal(err)
	}
	l, _ := st.ReadTaskList(ctx, other, "badger.md")
	if !strings.Contains(l.Content, "- [x] remote two") || !strings.Contains(l.Content, "- [ ] remote one") {
		t.Errorf("remote list after the edit = %q", l.Content)
	}
	items, err := app.ListTasks("", g.Locator)
	if err != nil || len(items) != 2 || !items[1].Done {
		t.Errorf("ListTasks over the locator = %+v, %v", items, err)
	}
}
