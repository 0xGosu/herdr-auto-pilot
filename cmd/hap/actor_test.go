package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// TestBuildAppResolvesTheActor: a front end acts as whoever HAP_ACTOR names,
// and a value it does not recognize fails the command before the store is
// opened — never a silent fallback to the operator.
func TestBuildAppResolvesTheActor(t *testing.T) {
	t.Setenv("HERDR_PANE_ID", "")
	paths := config.Paths{ConfigDir: t.TempDir(), StateDir: t.TempDir()}

	t.Setenv(domain.ActorEnv, "orchestrater")
	if _, _, err := buildApp(paths); err == nil {
		t.Fatal("buildApp with an unrecognized HAP_ACTOR must fail")
	}
	if entries, _ := os.ReadDir(paths.StateDir); len(entries) != 0 {
		t.Errorf("a refused actor must not open anything, state dir holds %d entries", len(entries))
	}

	for env, want := range map[string]string{"": domain.OperatorAuthor, "orchestrator": domain.OrchestratorAuthor} {
		t.Setenv(domain.ActorEnv, env)
		app, closeApp, err := buildApp(config.Paths{ConfigDir: t.TempDir(), StateDir: filepath.Join(t.TempDir(), "state")})
		if err != nil {
			t.Fatalf("buildApp with HAP_ACTOR=%q: %v", env, err)
		}
		closeApp()
		if app.Author != want {
			t.Errorf("HAP_ACTOR=%q: author = %q, want %q", env, app.Author, want)
		}
	}
}

// TestRunDaemonClearsTheActor: the daemon is never a front-end actor, and
// whatever it spawns inherits its environment — so `hap daemon` started by an
// orchestrator must drop HAP_ACTOR before anything else, or every LLM CLI and
// successor daemon it launches acts as the orchestrator.
func TestRunDaemonClearsTheActor(t *testing.T) {
	t.Setenv(domain.ActorEnv, domain.OrchestratorAuthor)
	paths := config.Paths{ConfigDir: t.TempDir(), StateDir: t.TempDir()}
	// --reload with no daemon running refuses at once; clearing happens first.
	if err := runDaemon(context.Background(), paths, io.Discard, []string{"--reload"}); err == nil {
		t.Fatal("runDaemon --reload with no daemon running must refuse")
	}
	if v, set := os.LookupEnv(domain.ActorEnv); set {
		t.Errorf("runDaemon left %s=%q in the environment its children inherit", domain.ActorEnv, v)
	}
}
