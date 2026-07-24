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

func TestMutateResolvesTildePath(t *testing.T) {
	// A ~-based task_sources.path must read AND write the real home file, not a
	// literally-named "~" directory. This is the functional half of the
	// expansion (TestLockPathExpandsShorthand covers the lock-key half).
	home := t.TempDir()
	t.Setenv("HOME", home)
	real := filepath.Join(home, "tasks.md")
	if err := os.WriteFile(real, []byte("- [ ] alpha\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mutate, _ := ReserveFirstPending("alpha")
	if _, err := Mutate("~/tasks.md", mutate); err != nil {
		t.Fatalf("Mutate(~/tasks.md): %v", err)
	}
	got := read(t, real)
	if got != "- [-] alpha\n" {
		t.Errorf("home file not mutated via ~ path: %q", got)
	}
	// And no stray literal-tilde file was created next to the cwd.
	if _, err := os.Stat("~"); err == nil {
		t.Error("a literal ~ path was created; expansion did not happen")
	}
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

// TestLockPathExpandsShorthand guards the lock-divergence invariant for the
// config shorthands ExpandPath resolves: a source spelled `~/tasks.md` or
// `$HOME/tasks.md` and its absolute form MUST hash to the same lock, or the
// daemon and the CLI/TUI would lock different keys for one file and stop
// serializing concurrent mutations.
func TestLockPathExpandsShorthand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	abs := filepath.Join(home, "tasks.md")
	want := LockPath(abs)
	for _, spelling := range []string{"~/tasks.md", "$HOME/tasks.md", "${HOME}/tasks.md"} {
		if got := LockPath(spelling); got != want {
			t.Errorf("LockPath(%q) = %q, want %q (same file as %q)", spelling, got, want, abs)
		}
	}
}

func TestReclaim(t *testing.T) {
	// The daemon's reclaim path takes back a hand-out the agent never started.
	// It may only touch an item that is still [-], and on a list repeating one
	// task it releases the recorded position or nothing at all — releasing "the
	// first [-] with this text" could clear a copy somebody else owns.
	const list = "- [ ] alpha\n- [-] beta\n- [x] gamma\n- [-] beta\n"
	tests := []struct {
		name    string
		index   int
		task    string
		want    string
		wantErr bool
	}{
		{
			name: "releases the recorded position",
			// The list repeats "beta"; the hand-out reserved #4, so #2 — which
			// may be somebody else's in-progress mark — must stay put.
			index: 4, task: "beta",
			want: "- [ ] alpha\n- [-] beta\n- [x] gamma\n- [ ] beta\n",
		},
		{
			name: "refuses an ambiguous duplicate when the position does not match",
			// The recorded position no longer carries the text, so the only
			// candidates are copies we cannot prove are ours — one of which may
			// be the operator's own in-progress mark. Fail closed.
			index: 3, task: "beta", wantErr: true,
		},
		{
			name: "refuses a duplicate when no position was recorded",
			// Rows written before the position was stored carry 0, which cannot
			// disambiguate a repeated text either.
			index: 0, task: "beta", wantErr: true,
		},
		{
			// Completed work must never be silently re-opened and re-handed out.
			name: "refuses a completed item", index: 3, task: "gamma", wantErr: true,
		},
		{
			// Already pending: nothing to take back, and the caller must be able
			// to tell that apart from a real reclaim for its audit row.
			name: "refuses a pending item", index: 1, task: "alpha", wantErr: true,
		},
		{
			name: "refuses a text that is gone", index: 1, task: "delta", wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Reclaim(tc.index, tc.task)(list)
			checkReclaim(t, got, err, tc.want, tc.wantErr)
		})
	}
}

func TestReclaimUniqueText(t *testing.T) {
	// With the text appearing once there is nothing to confuse, so a stale
	// position (the list renumbered under the reservation) is just an address
	// that missed — it must not veto the release.
	const list = "- [ ] alpha\n- [-] beta\n- [x] gamma\n"
	tests := []struct {
		name    string
		index   int
		task    string
		want    string
		wantErr bool
	}{
		{
			name:  "releases on the recorded position",
			index: 2, task: "beta",
			want: "- [ ] alpha\n- [ ] beta\n- [x] gamma\n",
		},
		{
			name:  "releases despite a stale position",
			index: 7, task: "beta",
			want: "- [ ] alpha\n- [ ] beta\n- [x] gamma\n",
		},
		{
			name:  "releases when no position was recorded",
			index: 0, task: "beta",
			want: "- [ ] alpha\n- [ ] beta\n- [x] gamma\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Reclaim(tc.index, tc.task)(list)
			checkReclaim(t, got, err, tc.want, tc.wantErr)
		})
	}
}

func checkReclaim(t *testing.T, got string, err error, want string, wantErr bool) {
	t.Helper()
	if wantErr {
		if err == nil {
			t.Fatalf("expected an error, got %q", got)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}
