package frontend_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
	"github.com/0xGosu/herdr-auto-pilot/internal/tasklocator"
)

// seedDatabaseSources writes a database-provider config with the given
// sources and one task in each named list, returning the loaded config.
func seedDatabaseSources(t *testing.T, app *frontend.App, body string, agents ...string) config.Config {
	t.Helper()
	if err := os.WriteFile(app.ConfigPath, []byte("[task_source_provider]\nprovider = \"database\"\n\n"+body), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, a := range agents {
		if _, _, err := app.AddTask(a, "", "task for "+a); err != nil {
			t.Fatalf("seed %s: %v", a, err)
		}
	}
	cfg, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func listExists(t *testing.T, st *store.Store, name string) bool {
	t.Helper()
	_, err := st.ReadTaskList(context.Background(), st.NodeID(), name)
	return err == nil
}

func TestRemoveTaskSourceAndListRemovesBoth(t *testing.T) {
	app, st := testApp(t)
	cfg := seedDatabaseSources(t, app,
		"[[task_sources]]\nagent = \"otter\"\npath = \"otter.md\"\n\n[[task_sources]]\nagent = \"heron\"\npath = \"heron.md\"\n",
		"otter", "heron")

	deleted, err := app.RemoveTaskSourceAndList(context.Background(), 0, cfg.TaskSources[0],
		tasklocator.DBLocator(st.NodeID(), "otter.md"), frontend.Status{})
	if err != nil || !deleted {
		t.Fatalf("RemoveTaskSourceAndList = %v, %v; want true, nil", deleted, err)
	}
	after, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	if len(after.TaskSources) != 1 || after.TaskSources[0].Agent != "heron" {
		t.Errorf("sources after = %+v, want only heron's", after.TaskSources)
	}
	if listExists(t, st, "otter.md") {
		t.Error("otter's list must be deleted")
	}
	if !listExists(t, st, "heron.md") {
		t.Error("an unrelated list must be untouched")
	}
}

// TestRemoveTaskSourceAndListRefusesASharedList: another source still hands
// out the same list, so deleting it would empty a live queue. Nothing may
// change — not even the source removal, which would otherwise have committed
// before the refusal.
func TestRemoveTaskSourceAndListRefusesASharedList(t *testing.T) {
	app, st := testApp(t)
	cfg := seedDatabaseSources(t, app,
		"[[task_sources]]\nagent = \"otter\"\npath = \"shared.md\"\n\n[[task_sources]]\nagent = \"heron\"\npath = \"shared.md\"\n",
		"otter")

	_, err := app.RemoveTaskSourceAndList(context.Background(), 0, cfg.TaskSources[0],
		tasklocator.DBLocator(st.NodeID(), "shared.md"), frontend.Status{})
	if err == nil || !strings.Contains(err.Error(), "task source #1 also uses") {
		t.Fatalf("err = %v, want a refusal naming source #1", err)
	}
	after, _ := app.Config()
	if len(after.TaskSources) != 2 {
		t.Errorf("a refused call must remove nothing, sources = %+v", after.TaskSources)
	}
	if !listExists(t, st, "shared.md") {
		t.Error("the shared list must survive")
	}
}

// TestRemoveTaskSourceAndListSeesADerivedSource: a derived (one list per
// agent) source names no list of its own, but for the agent whose derived name
// the list carries it IS that list — so a catch-all derived source left behind
// still counts as using it.
func TestRemoveTaskSourceAndListSeesADerivedSource(t *testing.T) {
	app, st := testApp(t)
	cfg := seedDatabaseSources(t, app,
		"[[task_sources]]\nagent = \"otter\"\npath = \"otter.md\"\n\n[[task_sources]]\nagent = \"\"\n",
		"otter")

	_, err := app.RemoveTaskSourceAndList(context.Background(), 0, cfg.TaskSources[0],
		tasklocator.DBLocator(st.NodeID(), "otter.md"), frontend.Status{})
	if err == nil || !strings.Contains(err.Error(), "task source #1 also uses") {
		t.Fatalf("err = %v, want the derived source counted as a user of otter.md", err)
	}
	if !listExists(t, st, "otter.md") {
		t.Error("the list must survive")
	}
}

func TestRemoveTaskSourceAndListRefusesStaleOrForeignTargets(t *testing.T) {
	app, st := testApp(t)
	cfg := seedDatabaseSources(t, app,
		"[[task_sources]]\nagent = \"otter\"\npath = \"otter.md\"\n", "otter")
	ctx := context.Background()

	t.Run("a file is not hap's to delete", func(t *testing.T) {
		_, err := app.RemoveTaskSourceAndList(ctx, 0, cfg.TaskSources[0], "/tmp/otter.md", frontend.Status{})
		if err == nil || !strings.Contains(err.Error(), "not kept in the hap database") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("a stale listing touches nothing", func(t *testing.T) {
		stale := cfg.TaskSources[0]
		stale.Path = "other.md"
		_, err := app.RemoveTaskSourceAndList(ctx, 0, stale, tasklocator.DBLocator(st.NodeID(), "otter.md"), frontend.Status{})
		if err == nil || !strings.Contains(err.Error(), "changed since it was listed") {
			t.Fatalf("err = %v", err)
		}
	})
	after, _ := app.Config()
	if len(after.TaskSources) != 1 || !listExists(t, st, "otter.md") {
		t.Errorf("refusals must leave the source and list in place: %+v", after.TaskSources)
	}
}

// TestRemoveTaskSourceAndListPerAgentSources: the per-agent form — one derived
// source per agent — is the common database setup. heron's source resolves to
// "otter.md" for otter (resolution never reads selectors), but it is scoped to
// another KNOWN agent name, so it does not use otter's list and D must work.
func TestRemoveTaskSourceAndListPerAgentSources(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	cfg := seedDatabaseSources(t, app,
		"[[task_sources]]\nagent = \"otter\"\n\n[[task_sources]]\nagent = \"heron\"\n")
	if _, err := st.EnsureTaskList(ctx, st.NodeID(), "otter.md", "otter", "# Tasks\n\n- [ ] a\n", time.Now()); err != nil {
		t.Fatal(err)
	}
	named := frontend.Status{AgentNamesKnown: true, AgentNames: map[string]string{"1": "otter", "2": "heron"}}

	deleted, err := app.RemoveTaskSourceAndList(ctx, 0, cfg.TaskSources[0],
		tasklocator.DBLocator(st.NodeID(), "otter.md"), named)
	if err != nil || !deleted {
		t.Fatalf("RemoveTaskSourceAndList = %v, %v; want true, nil", deleted, err)
	}
	if listExists(t, st, "otter.md") {
		t.Error("otter's list must be deleted")
	}
}

// TestRemoveTaskSourceAndListCountsATypeSelector is the control for the case
// above: a derived source scoped by agent TYPE feeds otter when otter is that
// type, so otter's list is still in use. And with nothing known about the
// selector at all, the answer must be the safe one — refuse.
func TestRemoveTaskSourceAndListCountsATypeSelector(t *testing.T) {
	for name, st := range map[string]frontend.Status{
		"live agent of that type": {
			AgentsKnown: true, AgentNamesKnown: true,
			AgentNames:      map[string]string{"1": "otter"},
			MonitoredAgents: []domain.AgentTransition{{AgentID: "1", AgentType: "claude"}},
		},
		"nothing known": {},
	} {
		t.Run(name, func(t *testing.T) {
			app, store := testApp(t)
			ctx := context.Background()
			cfg := seedDatabaseSources(t, app,
				"[[task_sources]]\nagent = \"otter\"\n\n[[task_sources]]\nagent = \"claude\"\n")
			if _, err := store.EnsureTaskList(ctx, store.NodeID(), "otter.md", "otter", "# Tasks\n\n- [ ] a\n", time.Now()); err != nil {
				t.Fatal(err)
			}
			_, err := app.RemoveTaskSourceAndList(ctx, 0, cfg.TaskSources[0],
				tasklocator.DBLocator(store.NodeID(), "otter.md"), st)
			if err == nil || !strings.Contains(err.Error(), "task source #1 also uses") {
				t.Fatalf("err = %v, want the type-scoped source counted", err)
			}
			if !listExists(t, store, "otter.md") {
				t.Error("the list must survive")
			}
		})
	}
}

// TestRemoveTaskSourceAndListRefusesAListTheSourceDoesNotUse: the locator must
// be the source's own list on this node — never another node's, and never one
// the source stopped using since it was listed.
func TestRemoveTaskSourceAndListRefusesAListTheSourceDoesNotUse(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	cfg := seedDatabaseSources(t, app,
		"[[task_sources]]\nagent = \"otter\"\npath = \"otter.md\"\n", "otter")
	for _, locator := range []string{
		tasklocator.DBLocator("b1b1b1b1b1b1b1b1", "otter.md"),
		tasklocator.DBLocator(st.NodeID(), "heron.md"),
	} {
		if _, err := app.RemoveTaskSourceAndList(ctx, 0, cfg.TaskSources[0], locator, frontend.Status{}); err == nil {
			t.Errorf("%s: want a refusal", locator)
		}
	}
	after, _ := app.Config()
	if len(after.TaskSources) != 1 || !listExists(t, st, "otter.md") {
		t.Errorf("refusals must leave the source and list in place: %+v", after.TaskSources)
	}
}
