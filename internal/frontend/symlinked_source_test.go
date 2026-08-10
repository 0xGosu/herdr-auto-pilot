package frontend_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
)

// TestSymlinkedSourcePathStillMatchesItsConfigEntry reproduces the macOS
// /var -> /private/var condition on any platform.
//
// The cap is looked up by comparing a resolved path against the configured
// [[task_sources]] path. When the resolution symlink-resolved one side and not
// the other, the lookup silently missed and the source ran UNCAPPED — the cap
// simply stopped applying, with nothing reporting it.
func TestSymlinkedSourcePathStillMatchesItsConfigEntry(t *testing.T) {
	app, _ := testApp(t)
	ctx := context.Background()

	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	// The operator configures the SYMLINKED spelling, as a macOS temp path is.
	listPath := filepath.Join(link, "tasks.md")
	if err := os.WriteFile(listPath, []byte("- [ ] one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := app.AddTaskSource(ctx, "brave-otter", "", listPath, "",
		frontend.MaxTasks(2)); err != nil {
		t.Fatal(err)
	}

	// Second add fills the cap; the third must be refused.
	if _, _, err := app.AddTask("brave-otter", "", "two"); err != nil {
		t.Fatalf("adding within the cap must succeed: %v", err)
	}
	_, _, err := app.AddTask("brave-otter", "", "three")
	if err == nil {
		t.Fatal("adding past max_tasks must be refused — a cap that silently stops " +
			"applying is worse than no cap, because the operator believes one is set")
	}
	if !strings.Contains(err.Error(), "cap 2") {
		t.Errorf("the refusal must name the cap that fired, got %v", err)
	}
}
