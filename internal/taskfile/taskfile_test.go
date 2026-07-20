package taskfile

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTasks(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tasks.md")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestReserveFirstPendingClaimsTheMatchingItem(t *testing.T) {
	// The daemon locates the item by text, not by a captured index: an item
	// further down the list is claimed when that is the one being delivered.
	path := writeTasks(t, "- [x] done\n- [ ] alpha\n- [ ] beta\n")
	mutate, claimed := ReserveFirstPending("beta")
	if _, err := Mutate(path, mutate); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if got, want := read(t, path), "- [x] done\n- [ ] alpha\n- [-] beta\n"; got != want {
		t.Errorf("file = %q, want %q", got, want)
	}
	if *claimed != 3 {
		t.Errorf("claimed index = %d, want 3 (positional across checked items)", *claimed)
	}
}

func TestReserveFirstPendingRefusesNonPendingTask(t *testing.T) {
	// "already claimed or completed" is exactly the case where the caller must
	// not send — two agents must never receive the same task.
	for _, tc := range []struct{ name, content string }{
		{"already in progress", "- [-] alpha\n"},
		{"already done", "- [x] alpha\n"},
		{"gone from the list", "- [ ] beta\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTasks(t, tc.content)
			mutate, _ := ReserveFirstPending("alpha")
			if _, err := Mutate(path, mutate); err == nil {
				t.Fatal("expected a refusal, got nil")
			}
			if got := read(t, path); got != tc.content {
				t.Errorf("file changed on a refused reserve: %q", got)
			}
		})
	}
}

func TestReserveFirstPendingSkipsAClaimedDuplicate(t *testing.T) {
	// A repeated task text: the first copy is already [-], so the claim lands
	// on the next pending copy rather than failing.
	path := writeTasks(t, "- [-] alpha\n- [ ] alpha\n")
	mutate, claimed := ReserveFirstPending("alpha")
	if _, err := Mutate(path, mutate); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if got, want := read(t, path), "- [-] alpha\n- [-] alpha\n"; got != want {
		t.Errorf("file = %q, want %q", got, want)
	}
	if *claimed != 2 {
		t.Errorf("claimed index = %d, want 2", *claimed)
	}
}

func TestReleaseIsClaimScoped(t *testing.T) {
	// Rollback returns the item this send reserved — and refuses anything
	// else, so a failed send can never re-open work somebody finished.
	t.Run("returns a reserved item to pending", func(t *testing.T) {
		path := writeTasks(t, "- [-] alpha\n")
		if _, err := Mutate(path, Release(1, "alpha")); err != nil {
			t.Fatalf("release: %v", err)
		}
		if got, want := read(t, path), "- [ ] alpha\n"; got != want {
			t.Errorf("file = %q, want %q", got, want)
		}
	})
	t.Run("refuses an item completed meanwhile", func(t *testing.T) {
		path := writeTasks(t, "- [x] alpha\n")
		if _, err := Mutate(path, Release(1, "alpha")); err == nil {
			t.Fatal("expected a refusal for a [x] item, got nil")
		}
		if got, want := read(t, path), "- [x] alpha\n"; got != want {
			t.Errorf("file = %q, want it untouched", got)
		}
	})
	t.Run("refuses when the text moved", func(t *testing.T) {
		path := writeTasks(t, "- [-] different\n")
		if _, err := Mutate(path, Release(1, "alpha")); err == nil {
			t.Fatal("expected a refusal when the text changed, got nil")
		}
	})
}

func TestMutatePreservesPermissions(t *testing.T) {
	// A user's 0644 --path checklist must not be narrowed on every edit.
	path := writeTasks(t, "- [ ] alpha\n")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	mutate, _ := ReserveFirstPending("alpha")
	if _, err := Mutate(path, mutate); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Errorf("perm = %o, want 644", got)
	}
}

func TestLockPathIsStableAcrossEquivalentPaths(t *testing.T) {
	// One physical file must hash to one lock, or concurrent mutations stop
	// serializing (macOS /var vs /private/var is the real-world case).
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.md")
	if err := os.WriteFile(path, []byte("- [ ] alpha\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	viaDot := filepath.Join(dir, ".", "tasks.md")
	if LockPath(path) != LockPath(viaDot) {
		t.Errorf("lock path differs for equivalent paths: %s vs %s", LockPath(path), LockPath(viaDot))
	}
}
