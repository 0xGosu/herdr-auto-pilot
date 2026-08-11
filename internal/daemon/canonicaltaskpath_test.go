package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/tasklocator"
)

// TestCanonicalTaskPathDelegatesToTasklocator pins that the daemon shares ONE
// normalization with taskfile.LockPath and the TUI's task grouping.
//
// Three copies of this logic existed before; the hazard of them drifting is
// silent rather than loud. filepath.Abs does not fail on a scheme'd locator —
// it returns "<cwd>/gist:/id/f.md" — and each hap process has a different
// working directory, so a local copy that forgot the scheme branch would have
// the daemon's reservations and the TUI's grouping key the same source
// differently, with nothing erroring anywhere.
func TestCanonicalTaskPathDelegatesToTasklocator(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "tasks.md")
	if err := os.WriteFile(real, []byte("- [ ] a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HAPTEST_DAEMON_DIR", dir)

	for _, in := range []string{
		real,
		"$HAPTEST_DAEMON_DIR/tasks.md",
		filepath.Join(dir, ".", "tasks.md"),
		"gist://3f2a1b9c/brave-otter.md",
		"gist://3f2a1b9c/shared-backlog.md",
	} {
		if got, want := canonicalTaskPath(in), tasklocator.Canonical(in); got != want {
			t.Errorf("canonicalTaskPath(%q) = %q, want %q — the daemon must not carry its own "+
				"copy of this normalization", in, got, want)
		}
	}
}

// TestCanonicalTaskPathLeavesARemoteLocatorAlone is the specific regression:
// a gist locator must survive without acquiring the process's cwd.
func TestCanonicalTaskPathLeavesARemoteLocatorAlone(t *testing.T) {
	const locator = "gist://3f2a1b9c/brave-otter.md"
	got := canonicalTaskPath(locator)
	if got != locator {
		t.Fatalf("canonicalTaskPath(%q) = %q — a remote locator must not be rewritten", locator, got)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.IsAbs(got) || len(got) > 0 && got[0] == filepath.Separator {
		t.Errorf("the locator became a filesystem path: %q", got)
	}
	if want := filepath.Join(cwd, "gist:", "3f2a1b9c", "brave-otter.md"); got == want {
		t.Errorf("the locator picked up the working directory: %q", got)
	}
}
