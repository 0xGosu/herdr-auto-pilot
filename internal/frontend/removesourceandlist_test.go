package frontend_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
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
		tasklocator.DBLocator(st.NodeID(), "otter.md"))
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
		tasklocator.DBLocator(st.NodeID(), "shared.md"))
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
		tasklocator.DBLocator(st.NodeID(), "otter.md"))
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
		_, err := app.RemoveTaskSourceAndList(ctx, 0, cfg.TaskSources[0], "/tmp/otter.md")
		if err == nil || !strings.Contains(err.Error(), "not kept in the hap database") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("a stale listing touches nothing", func(t *testing.T) {
		stale := cfg.TaskSources[0]
		stale.Path = "other.md"
		_, err := app.RemoveTaskSourceAndList(ctx, 0, stale, tasklocator.DBLocator(st.NodeID(), "otter.md"))
		if err == nil || !strings.Contains(err.Error(), "changed since it was listed") {
			t.Fatalf("err = %v", err)
		}
	})
	after, _ := app.Config()
	if len(after.TaskSources) != 1 || !listExists(t, st, "otter.md") {
		t.Errorf("refusals must leave the source and list in place: %+v", after.TaskSources)
	}
}
