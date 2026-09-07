package cli_test

import (
	"context"
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// The tests in this file exercise the DEFAULT task store, which a fresh
// install runs on without configuring anything. Every other CLI test that
// touches a checklist declares local_fs, because its list is a file on disk —
// and a suite where that is the only shape is exactly how the
// locator-is-not-a-path bugs kept shipping green. These are the counterweight:
// they never write a file, and a locator here is a `db://` URI that os.Stat can
// only fail on.

// TestFreshInstallAddsAPathlessSourceAndRoundTripsItsTasks is the end-to-end
// evidence for the default: a source created with no path at all, a list
// created on demand, and every `hap task` verb reading and writing it through
// the store.
func TestFreshInstallAddsAPathlessSourceAndRoundTripsItsTasks(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	// A derived source names one list per SEEN agent, so the agent has to
	// exist before its list can be created on demand.
	if err := st.AssignAgentName(ctx, "w1:p1", "brave-otter"); err != nil {
		t.Fatal(err)
	}
	seedRoster(t, st, domain.AgentTransition{
		AgentID: "w1:p1", PaneID: "w1:p1", AgentType: "claude", Status: "idle",
	})

	// No positional checklist argument: under the default the file name is
	// derived per matched agent.
	if _, err := run(t, app, "config", "task-source", "add", "--agent", "brave-otter"); err != nil {
		t.Fatalf("a pathless source must be creatable on a fresh install: %v", err)
	}
	cfg, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.TaskSources) != 1 {
		t.Fatalf("got %d sources, want 1", len(cfg.TaskSources))
	}
	if got := cfg.TaskSources[0].Path; got != "" {
		t.Errorf("path = %q, want it left empty so the name derives per agent", got)
	}
	// The inheritance is a live link and must never be materialized onto the
	// entry — that is what lets the operator move every source at once.
	if got := cfg.TaskSources[0].Provider; got != "" {
		t.Errorf("provider = %q, want the inheritance left empty", got)
	}
	if got := cfg.ResolveProvider(cfg.TaskSources[0]).Name; got != config.ProviderSQLite {
		t.Errorf("resolved provider = %q, want %q", got, config.ProviderSQLite)
	}

	for _, item := range []string{"alpha", "beta"} {
		if _, err := run(t, app, "task", "brave-otter", "add", item); err != nil {
			t.Fatalf("add %q: %v", item, err)
		}
	}
	out, err := run(t, app, "task", "brave-otter", "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "alpha") || !strings.Contains(out, "beta") {
		t.Errorf("the database-backed list must show its items:\n%s", out)
	}
	if _, err := run(t, app, "task", "brave-otter", "done", "2"); err != nil {
		t.Fatal(err)
	}
	out, err = run(t, app, "task", "brave-otter", "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[x]") && !strings.Contains(out, "done") {
		t.Errorf("completing an item must survive the round trip:\n%s", out)
	}
}

// TestFreshInstallRefusalNamesTheWayOut: pointing the default at a markdown
// file is the first thing a new operator tries, and the refusal has to say
// what to type instead — not only what is wrong.
func TestFreshInstallRefusalNamesTheWayOut(t *testing.T) {
	app, _ := testApp(t)
	_, err := run(t, app, "config", "task-source", "add", "--agent", "brave-otter", "/tmp/tasks.md")
	if err == nil {
		t.Fatal("a filesystem path under the sqlite provider must be refused")
	}
	for _, want := range []string{"--provider " + config.ProviderLocalFS, "omit the path"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must mention %q, got: %v", want, err)
		}
	}
}
