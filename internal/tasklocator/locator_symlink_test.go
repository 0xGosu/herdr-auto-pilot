package tasklocator_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/tasklocator"
)

// TestLocalLocatorIsNotSymlinkResolved reproduces the macOS /var -> /private/var
// condition. The locator must read as the operator wrote it, while Canonical
// still collapses both spellings — that split is what keeps the max_tasks
// lookup and {task_list_path} correct without weakening lock identity.
func TestLocalLocatorIsNotSymlinkResolved(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	viaLink := filepath.Join(link, "tasks.md")
	if err := os.WriteFile(filepath.Join(real, "tasks.md"), []byte("- [ ] a\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := tasklocator.Resolve(config.Default(),
		config.TaskSource{Agent: "a", Path: viaLink}, "a")
	if err != nil {
		t.Fatal(err)
	}
	if res.Locator != viaLink {
		t.Errorf("Locator = %q, want the operator's spelling %q — a resolved one stops "+
			"matching their [[task_sources]] path", res.Locator, viaLink)
	}
	if res.Display != viaLink {
		t.Errorf("Display = %q, want %q", res.Display, viaLink)
	}
	// Identity still collapses both spellings.
	if tasklocator.Canonical(viaLink) != tasklocator.Canonical(filepath.Join(real, "tasks.md")) {
		t.Error("Canonical must still resolve both spellings to one identity")
	}
}
