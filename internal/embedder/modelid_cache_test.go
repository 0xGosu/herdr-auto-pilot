package embedder

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// freshProcess forgets the in-process ids, as a new hap process starts with,
// and counts the digests computed from here on.
func freshProcess(t *testing.T, dir string) *int {
	t.Helper()
	modelIDMu.Lock()
	prevCache, prevDir, prevFn := modelIDCache, modelIDDir, modelHashFn
	modelIDCache, modelIDDir = map[string]string{}, dir
	n := new(int)
	modelHashFn = func(p string) (string, modelIDEntry, bool, error) { *n++; return hashModel(p) }
	modelIDMu.Unlock()
	t.Cleanup(func() {
		modelIDMu.Lock()
		modelIDCache, modelIDDir, modelHashFn = prevCache, prevDir, prevFn
		modelIDMu.Unlock()
	})
	return n
}

// TestModelIDIsHashedOncePerMachineNotPerProcess pins the one-shot CLI's
// biggest cost: every `hap status` / `hap agents` streamed the whole model
// through SHA-256 to report embedding drift. A later process reuses the id
// while the file is unchanged — and re-hashes the moment it is not, which the
// per-process cache alone never did for a model replaced in place.
func TestModelIDIsHashedOncePerMachineNotPerProcess(t *testing.T) {
	state, models := t.TempDir(), t.TempDir()
	model := filepath.Join(models, "m.gguf")
	if err := os.WriteFile(model, []byte("model bytes v1"), 0o600); err != nil {
		t.Fatal(err)
	}

	hashed := freshProcess(t, state)
	first := ModelIDFor(model)
	if *hashed != 1 {
		t.Fatalf("first process hashed %d times, want 1", *hashed)
	}

	hashed = freshProcess(t, state)
	if got := ModelIDFor(model); got != first {
		t.Fatalf("second process got %q, want the cached %q", got, first)
	}
	if *hashed != 0 {
		t.Errorf("second process re-hashed an unchanged model (%d times)", *hashed)
	}

	// Replaced in place: same path, new content, later mtime.
	if err := os.WriteFile(model, []byte("model bytes version 2"), 0o600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(model, later, later); err != nil {
		t.Fatal(err)
	}
	hashed = freshProcess(t, state)
	second := ModelIDFor(model)
	if *hashed != 1 || second == first {
		t.Errorf("a replaced model: hashed %d times, id %q (first %q) — want one re-hash and a new id",
			*hashed, second, first)
	}

	// Replaced by rename with identical size and mtime: only the inode tells.
	other := filepath.Join(models, "other.gguf")
	if err := os.WriteFile(other, []byte("model bytes version 3"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(other, later, later); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(other, model); err != nil {
		t.Fatal(err)
	}
	hashed = freshProcess(t, state)
	third := ModelIDFor(model)
	if inode, _, _ := fileIdentity(mustStat(t, model)); inode != 0 {
		if *hashed != 1 || third == second {
			t.Errorf("a model swapped by rename: hashed %d times, id %q (was %q)", *hashed, third, second)
		}
	}

	// Rewritten IN PLACE at the same size with its mtime put back — `cp -p`,
	// `touch -r`, an archive tool storing whole seconds. Size, mtime, inode and
	// device all survive; only the change time, which nothing in user space can
	// set, says the bytes are different.
	mtime := mustStat(t, model).ModTime()
	// File timestamps come from the kernel's COARSE clock (a few ms per tick
	// on Linux): a rewrite in the same tick as the rename above would share its
	// ctime. A real replacement lands long after the hash it invalidates.
	time.Sleep(50 * time.Millisecond)
	if err := os.WriteFile(model, []byte("model bytes version 4"), 0o600); err != nil { // same length as v3
		t.Fatal(err)
	}
	if err := os.Chtimes(model, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	hashed = freshProcess(t, state)
	if fourth := ModelIDFor(model); *hashed != 1 || fourth == third {
		t.Errorf("a same-size rewrite with its mtime restored: hashed %d times, id %q (was %q) — a stale id would outlive the restart",
			*hashed, fourth, third)
	}
}

// TestModelIDWithoutACacheDirHashesEveryProcess is the control: without the
// directory nothing persists, exactly the behaviour before.
func TestModelIDWithoutACacheDirHashesEveryProcess(t *testing.T) {
	model := filepath.Join(t.TempDir(), "m.gguf")
	if err := os.WriteFile(model, []byte("bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		hashed := freshProcess(t, "")
		ModelIDFor(model)
		if *hashed != 1 {
			t.Fatalf("process %d hashed %d times, want 1", i, *hashed)
		}
	}
}

// TestAnUnreadableModelIDCacheOnlyCostsAHash: a corrupt cache file is ignored,
// the id is computed, and the file is rewritten usable.
func TestAnUnreadableModelIDCacheOnlyCostsAHash(t *testing.T) {
	state := t.TempDir()
	model := filepath.Join(t.TempDir(), "m.gguf")
	if err := os.WriteFile(model, []byte("bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, modelIDFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	hashed := freshProcess(t, state)
	id := ModelIDFor(model)
	if *hashed != 1 || id == "" {
		t.Fatalf("corrupt cache: hashed %d, id %q", *hashed, id)
	}
	hashed = freshProcess(t, state)
	if ModelIDFor(model) != id || *hashed != 0 {
		t.Errorf("the cache was not rewritten usable (hashed %d)", *hashed)
	}
}

func mustStat(t *testing.T, p string) os.FileInfo {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi
}
