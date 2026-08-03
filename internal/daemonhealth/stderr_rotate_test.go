package daemonhealth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withStderrFile points os.Stderr at a real file in dir, restoring it after.
func withStderrFile(t *testing.T, dir string) *os.File {
	t.Helper()
	f, err := os.OpenFile(StderrLogPath(dir), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = f
	t.Cleanup(func() {
		os.Stderr = orig
		f.Close()
	})
	return f
}

// TestRotateOwnStderrRotatesWhileHeldOpen is the case OpenStderrLog cannot
// cover: a daemon that never restarts. The cap has to hold for the process
// currently writing, not only at the next spawn.
func TestRotateOwnStderrRotatesWhileHeldOpen(t *testing.T) {
	dir := t.TempDir()
	f := withStderrFile(t, dir)

	// "head" then filler then "TAIL-MARKER": the tail is what a crash trail is,
	// so that is what must survive.
	if _, err := f.WriteString("HEAD-MARKER" + strings.Repeat("a", StderrLogCap+1024)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("TAIL-MARKER"); err != nil {
		t.Fatal(err)
	}
	RotateOwnStderrIfNeeded(dir)

	old, err := os.ReadFile(StderrLogPath(dir) + ".old")
	if err != nil {
		t.Fatalf("the oversized log must be preserved as .old: %v", err)
	}
	// Bounded by design: the copy is capped, so an enormous log cannot be
	// slurped on the very path that exists for the disk-full case.
	if int64(len(old)) > StderrLogCap {
		t.Errorf(".old holds %d bytes, past the %d cap", len(old), StderrLogCap)
	}
	if !strings.Contains(string(old), "TAIL-MARKER") {
		t.Error(".old must keep the TAIL of the log — that is where a crash lands")
	}
	if strings.Contains(string(old), "HEAD-MARKER") {
		t.Error(".old kept the head; the copy should have been the trailing window")
	}
	fi, err := os.Stat(StderrLogPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != 0 {
		t.Errorf("live log is %d bytes after rotation, want 0", fi.Size())
	}

	// The still-open descriptor must keep writing into the LIVE file. This is
	// why rotation truncates rather than renames: a rename would leave fd 2
	// bound to the .old inode and the fresh file empty forever.
	if _, err := f.WriteString("after-rotation\n"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(StderrLogPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "after-rotation") {
		t.Errorf("writes after rotation did not reach the live log, got %q", data)
	}
	// O_APPEND re-seeks to the (now zero) end, so no sparse gap is left.
	if len(data) != len("after-rotation\n") {
		t.Errorf("live log is %d bytes, want %d — a sparse gap was left",
			len(data), len("after-rotation\n"))
	}
}

func TestRotateOwnStderrLeavesSmallLogAlone(t *testing.T) {
	dir := t.TempDir()
	f := withStderrFile(t, dir)
	if _, err := f.WriteString("small\n"); err != nil {
		t.Fatal(err)
	}
	RotateOwnStderrIfNeeded(dir)

	if _, err := os.Stat(StderrLogPath(dir) + ".old"); !os.IsNotExist(err) {
		t.Error("an under-cap log must not rotate")
	}
	data, _ := os.ReadFile(StderrLogPath(dir))
	if string(data) != "small\n" {
		t.Errorf("under-cap log was modified, got %q", data)
	}
}

// TestRotateOwnStderrIgnoresNonRegularStderr: a foreground daemon's stderr is a
// terminal and a test's may be a pipe. Neither is ours to truncate.
func TestRotateOwnStderrIgnoresNonRegularStderr(t *testing.T) {
	dir := t.TempDir()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()

	orig := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	RotateOwnStderrIfNeeded(dir) // must not panic or create anything
	if _, err := os.Stat(filepath.Join(dir, StderrLogName+".old")); !os.IsNotExist(err) {
		t.Error("a pipe stderr must not produce a rotation")
	}
}

// TestRotateOwnStderrSkipsWhenPathIsADifferentInode: another process already
// rotated, so those bytes are not ours to copy over its .old.
func TestRotateOwnStderrSkipsWhenPathIsADifferentInode(t *testing.T) {
	dir := t.TempDir()
	f := withStderrFile(t, dir)
	if _, err := f.WriteString(strings.Repeat("a", StderrLogCap+1024)); err != nil {
		t.Fatal(err)
	}
	// Someone else rotated: the path now names a fresh, different file.
	if err := os.Rename(StderrLogPath(dir), filepath.Join(dir, "moved-away")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(StderrLogPath(dir), []byte("theirs\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	RotateOwnStderrIfNeeded(dir)

	if _, err := os.Stat(StderrLogPath(dir) + ".old"); !os.IsNotExist(err) {
		t.Error("must not clobber another process's rotation")
	}
	data, _ := os.ReadFile(StderrLogPath(dir))
	if string(data) != "theirs\n" {
		t.Errorf("the other process's live log was modified, got %q", data)
	}
}
