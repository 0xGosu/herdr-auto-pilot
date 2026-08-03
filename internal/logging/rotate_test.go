package logging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotatingFileCapsGrowthWhileHeldOpen(t *testing.T) {
	// The regression: the daemon holds this file open for its whole life, so a
	// rotation that only ran at open time never fired for the process writing
	// nearly all the volume (a live state dir reached 1.9 GB).
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")
	r, err := newRotatingFile(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	line := []byte(strings.Repeat("x", 30) + "\n")
	for range 20 {
		if _, err := r.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	live, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if live.Size() > 100 {
		t.Errorf("live log grew past the cap: %d bytes", live.Size())
	}
	old, err := os.Stat(path + ".old")
	if err != nil {
		t.Fatalf("rotated sibling missing: %v", err)
	}
	if old.Size() == 0 {
		t.Error("rotated sibling is empty; the previous log was lost")
	}
}

func TestRotatingFileKeepsOnlyOneOldSibling(t *testing.T) {
	// Bounded disk use is the whole point: many rotations must still leave at
	// most the live file plus one ".old".
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")
	r, err := newRotatingFile(path, 50)
	if err != nil {
		t.Fatal(err)
	}
	for range 50 {
		if _, err := r.Write([]byte(strings.Repeat("y", 40) + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("want live log + one .old, got %v", names)
	}
}

func TestRotatingFileReattachesAfterForeignRotation(t *testing.T) {
	// Every hap process appends to this one file. When another rotates it, this
	// handle must follow the live path instead of writing on into an inode
	// nobody reads any more.
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")
	r, err := newRotatingFile(path, 40)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Write([]byte("first\n")); err != nil {
		t.Fatal(err)
	}
	// Another process rotates underneath us.
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Write([]byte(strings.Repeat("z", 60) + "\n")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("live log not recreated: %v", err)
	}
	if !strings.Contains(string(data), "z") {
		t.Errorf("write landed off the live path, got %q", data)
	}
	// The foreign rotation's content must survive — it must not be renamed
	// over by ours.
	old, err := os.ReadFile(path + ".old")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(old), "first") {
		t.Errorf(".old was clobbered, got %q", old)
	}
}

func TestSetupLogsToRotatingFile(t *testing.T) {
	dir := t.TempDir()
	logger, err := Setup(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("hello")
	data, err := os.ReadFile(filepath.Join(dir, "herd-auto-prompter.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "hello") {
		t.Errorf("log line not written, got %q", data)
	}
}
