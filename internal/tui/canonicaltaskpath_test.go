package tui

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/tasklocator"
)

// TestCanonicalTaskPathDelegatesToTasklocator is the TUI half of the
// three-way agreement (see the daemon's copy of this test and
// taskfile.LockPath). The TUI dedupes marked task targets by this key and the
// daemon reserves by it, so a divergence would let a bulk action mutate one
// list line twice, or let the TUI show two groups for one list.
func TestCanonicalTaskPathDelegatesToTasklocator(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "tasks.md")
	if err := os.WriteFile(real, []byte("- [ ] a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HAPTEST_TUI_DIR", dir)

	for _, in := range []string{
		real,
		"$HAPTEST_TUI_DIR/tasks.md",
		filepath.Join(dir, ".", "tasks.md"),
		"gist://3f2a1b9c/brave-otter.md",
	} {
		if got, want := canonicalTaskPath(in), tasklocator.Canonical(in); got != want {
			t.Errorf("canonicalTaskPath(%q) = %q, want %q — the TUI must not carry its own "+
				"copy of this normalization", in, got, want)
		}
	}
}

func TestCanonicalTaskPathLeavesARemoteLocatorAlone(t *testing.T) {
	const locator = "gist://3f2a1b9c/brave-otter.md"
	if got := canonicalTaskPath(locator); got != locator {
		t.Errorf("canonicalTaskPath(%q) = %q — a remote locator must not be rewritten "+
			"into a path relative to the TUI's working directory", locator, got)
	}
}
