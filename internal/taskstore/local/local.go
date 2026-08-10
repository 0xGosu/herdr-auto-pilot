// Package local implements ports.TaskStore over the filesystem — the default,
// and the only behaviour hap had before task lists gained storage providers.
//
// It is a thin wrapper over internal/taskfile ON PURPOSE. The locked
// read-modify-write cycle, the advisory lock keyed on the canonical locator,
// the permission-preserving atomic write and the claim rules all keep their one
// implementation there; this package only adapts their signatures to the port
// and adds create-on-demand, which taskfile deliberately does not do.
package local

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
	"github.com/0xGosu/herdr-auto-pilot/internal/taskfile"
)

// Store reads and writes task lists on this machine.
type Store struct{}

var (
	_ ports.TaskStore     = (*Store)(nil)
	_ ports.EnsureCreator = (*Store)(nil)
)

// New returns the filesystem task store. It holds no state, so every caller may
// share one.
func New() *Store { return &Store{} }

// Read returns the checklist's bytes.
//
// ctx is accepted for the port and deliberately unused: a local read is a
// single os.ReadFile with no cancellation point worth wiring, and pretending
// otherwise would suggest a timeout that does not exist.
func (s *Store) Read(_ context.Context, locator string) ([]byte, error) {
	// ExpandPath here rather than in the caller: reads bypass the taskfile
	// package (which expands its own writes and locks), so without this a
	// source spelled "~/tasks.md" would read a different file than it locks.
	return os.ReadFile(config.ExpandPath(locator))
}

// Mutate applies fn as one locked read-modify-write.
func (s *Store) Mutate(_ context.Context, locator string, wait time.Duration,
	fn func(content string) (string, error)) ([]domain.ChecklistItem, error) {

	return taskfile.MutateWithin(locator, wait, fn)
}

// Ensure creates the checklist with initial content when it is missing.
//
// It runs UNDER the same advisory lock every mutation takes, and creates with
// O_EXCL. Both matter: the caller this replaces did a bare os.Stat and then an
// os.WriteFile, so two concurrent generated-task confirms could both miss the
// stat and both write, and the second one's write discarded the first one's
// content.
func (s *Store) Ensure(_ context.Context, locator, initial string) (bool, error) {
	path := config.ExpandPath(locator)

	lockPath := taskfile.LockPath(locator)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return false, err
	}
	unlock, err := taskfile.Lock(lockPath)
	if err != nil {
		return false, err
	}
	defer unlock()

	if _, err := os.Stat(path); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		// An existing-but-unreadable list must NOT read as "absent": creating
		// over it would discard the operator's checklist. Surface the error.
		return false, fmt.Errorf("stat task list %s: %w", path, err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			// Lost a race with a process that does not take our lock. Its
			// content wins; ours was only a starting point.
			return false, nil
		}
		return false, err
	}
	if _, err := f.WriteString(initial); err != nil {
		f.Close()
		return false, err
	}
	if err := f.Close(); err != nil {
		return false, err
	}
	return true, nil
}
