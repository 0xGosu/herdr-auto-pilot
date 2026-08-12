package frontend_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// TestBootstrapWritesWhereItRegisters is the regression guard for a split-brain
// bug: the confirm wrote the generated tasks to a LOCAL <state>/tasks/<name>.md
// while registering a source that pointed at the store, so the agent's real
// list stayed empty and nothing anywhere reported a problem.
//
// It is asserted through config rather than through the gist, because the point
// is the AGREEMENT between the two — the file the confirm writes and the source
// it registers must resolve to the same list.
func TestBootstrapWritesWhereItRegisters(t *testing.T) {
	t.Run("local default writes the state-dir file and registers it", func(t *testing.T) {
		app, st := testApp(t)
		app.Herdr = &fakeHerdr{}
		stateDir := t.TempDir()
		app.StateDir = stateDir
		ctx := context.Background()

		name, _ := st.EnsureAgentName(ctx, "w1:p1")
		id, _ := st.AppendAudit(ctx, domain.AuditRecord{
			AgentID: "w1:p1", SituationType: domain.SituationIdle, Trigger: "t",
			Action: "escalated", Status: "escalated",
			Suggestion: domain.SuggestTaskPrefix + "Task A", CreatedAt: time.Now(),
		})
		if err := app.Confirm(ctx, id, false); err != nil {
			t.Fatal(err)
		}

		want := filepath.Join(stateDir, "tasks", name+".md")
		if _, err := os.Stat(want); err != nil {
			t.Fatalf("the bootstrap file is missing: %v", err)
		}
		cfg, err := config.Load(app.ConfigPath)
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.TaskSources) != 1 {
			t.Fatalf("got %d sources, want 1", len(cfg.TaskSources))
		}
		if got := cfg.TaskSources[0].Path; got != want {
			t.Errorf("registered path = %q, want the file that was written (%q)", got, want)
		}
	})

	t.Run("remote default registers the store file, not the state-dir path", func(t *testing.T) {
		app, st := testApp(t)
		app.Herdr = &fakeHerdr{}
		stateDir := t.TempDir()
		app.StateDir = stateDir
		ctx := context.Background()

		// A remote default with no reachable backend: the confirm must FAIL
		// rather than quietly writing the tasks somewhere else. That failure is
		// the assertion — before the fix it "succeeded" and wrote locally.
		for _, kv := range [][2]string{
			{"task_source_provider.provider", "github_gist"},
			{"task_source_provider.github_gist.gist_id", "3f2a1b9c"},
		} {
			if _, err := app.SetField(ctx, kv[0], kv[1]); err != nil {
				t.Fatal(err)
			}
		}

		name, _ := st.EnsureAgentName(ctx, "w2:p2")
		id, _ := st.AppendAudit(ctx, domain.AuditRecord{
			AgentID: "w2:p2", SituationType: domain.SituationIdle, Trigger: "t",
			Action: "escalated", Status: "escalated",
			Suggestion: domain.SuggestTaskPrefix + "Task A", CreatedAt: time.Now(),
		})
		err := app.Confirm(ctx, id, false)
		if err == nil {
			t.Fatal("a confirm against an unreachable remote store must FAIL — writing the " +
				"tasks to a local file while registering a store source leaves the agent " +
				"with an empty list and nothing reporting it")
		}
		// It must fail for the RIGHT reason: reaching the store, not a path bug.
		if !strings.Contains(err.Error(), "env_file") && !strings.Contains(err.Error(), "gist") {
			t.Errorf("want a store-side failure, got %v", err)
		}
		// And it must NOT have written the local bootstrap file.
		if _, statErr := os.Stat(filepath.Join(stateDir, "tasks", name+".md")); statErr == nil {
			t.Error("the confirm wrote a local bootstrap file under a remote provider")
		}
	})
}
