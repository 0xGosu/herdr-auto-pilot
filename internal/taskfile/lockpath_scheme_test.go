package taskfile

import (
	"path/filepath"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/tasklocator"
)

// TestLockPathKeysOnTheCanonicalLocator is the taskfile third of the three-way
// agreement (see internal/daemon and internal/tui for the others): the lock
// key, the daemon's claim key and the TUI's grouping key must all come from
// tasklocator.Canonical.
func TestLockPathKeysOnTheCanonicalLocator(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "tasks.md")
	t.Setenv("HAPTEST_LOCK_DIR", dir)

	// Two spellings of one filesystem list share a lock.
	if got, want := LockPath("$HAPTEST_LOCK_DIR/tasks.md"), LockPath(real); got != want {
		t.Errorf("two spellings of one file must share a lock: %q vs %q", got, want)
	}
	// Two DIFFERENT lists do not.
	if LockPath(real) == LockPath(filepath.Join(dir, "other.md")) {
		t.Error("different lists must not share a lock")
	}
}

// TestLockPathSerializesARemoteLocator is what makes a gist read-modify-write
// safe between hap processes on this host. A gist has no compare-and-swap, so
// this lock is the ONLY thing standing between two hap processes and a
// last-write-wins overwrite.
func TestLockPathSerializesARemoteLocator(t *testing.T) {
	const a = "gist://3f2a1b9c/brave-otter.md"
	const b = "gist://3f2a1b9c/calm-badger.md"

	if LockPath(a) != LockPath(a) {
		t.Error("the lock key for one remote list must be stable")
	}
	if LockPath(a) == LockPath(b) {
		t.Error("two files in one gist are two lists and must not share a lock")
	}
	// And it must be keyed on the locator as written, not on a path derived
	// from the process's working directory.
	if LockPath(a) != LockPath(tasklocator.Canonical(a)) {
		t.Error("LockPath must key on the canonical locator")
	}
}
