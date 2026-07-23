// Package taskfile owns the locked read-modify-write cycle over a declared
// task-source checklist. Every hap process mutates the same files — the CLI
// (`hap task …`), the TUI, and the daemon's auto-send — so the lock, the
// atomic write, and the reserve/release claim rules must have exactly one
// implementation. The pure text transforms live in internal/domain; this
// package only adds the file I/O around them.
package taskfile

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// Mutate applies fn to the file's content as one locked read-modify-write and
// returns the resulting checklist. The file's existing permission bits are
// preserved (a user's 0644 --path file must not be narrowed to 0600 on every
// edit) and the write is atomic, so a concurrent reader sees either the old or
// the new content, never a partial write.
func Mutate(path string, fn func(content string) (string, error)) ([]domain.ChecklistItem, error) {
	return MutateWithin(path, 0, fn)
}

// MutateWithin is Mutate with a bounded wait for the file lock: it gives up
// with an error rather than blocking once wait has elapsed (wait <= 0 blocks,
// i.e. plain Mutate). The daemon uses it because its reserve-before-send runs
// on the main select loop, where an unbounded wait behind another hap process
// would stall every agent.
func MutateWithin(path string, wait time.Duration, fn func(content string) (string, error)) ([]domain.ChecklistItem, error) {
	lockPath := LockPath(path)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return nil, err
	}
	unlock, err := LockWithin(lockPath, wait)
	if err != nil {
		return nil, err
	}
	defer unlock()

	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out, err := fn(string(data))
	if err != nil {
		return nil, err
	}
	if err := WriteFileAtomic(path, []byte(out), info.Mode().Perm()); err != nil {
		return nil, err
	}
	return domain.ParseChecklist(out), nil
}

// WriteFileAtomic writes data to path via a temp file in the same directory
// then renames it into place, so a concurrent reader (the daemon) sees either
// the old or the new file, never a partial write.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".hap-task-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // best-effort cleanup; a no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// LockPath returns a stable, hap-owned lock-file path for a task file, keyed
// by the file's canonical path. Keeping the lock in a shared temp dir — rather
// than a `<file>.lock` sidecar — serializes concurrent mutations without
// dropping a stray lock file into the user's repo next to a --path checklist.
//
// The path is canonicalized (absolute + symlinks resolved, best-effort) so
// every caller — the daemon, the CLI, the TUI's add/edit, and the TUI's bulk
// toggle/delete (which passes an already symlink-resolved path) — hashes to
// the SAME key for one physical file. Without this, a symlinked path component
// (e.g. macOS /var vs /private/var) would yield two different locks and stop
// serializing concurrent mutations of the same checklist.
func LockPath(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	sum := sha256.Sum256([]byte(path))
	return filepath.Join(os.TempDir(), "hap-task-locks", hex.EncodeToString(sum[:16])+".lock")
}

// ExpectText guards a checklist mutation against a file that changed while the
// caller had a prompt or confirmation open: inside the same locked
// read-modify-write, it verifies task #index still carries exactly the text the
// caller resolved the number against. Task numbers are positional and renumber
// on every delete, so without this a stale index would silently mutate a
// different line.
func ExpectText(content string, index int, want string) error {
	for _, it := range domain.ParseChecklist(content) {
		if it.Index != index {
			continue
		}
		if it.Text != want {
			return fmt.Errorf("task #%d is now %q, not %q — the checklist changed; refresh and retry", index, it.Text, want)
		}
		return nil
	}
	return fmt.Errorf("task #%d no longer exists — the checklist changed; refresh and retry", index)
}

// Reserve claims item index for delivery: it verifies the item still carries
// exactly taskText AND is still pending, then marks it [-], as ONE locked
// read-modify-write. Checking and claiming must be atomic — a concurrent edit
// slipping between them is what would let the same task be delivered twice.
func Reserve(index int, taskText string) func(string) (string, error) {
	return func(content string) (string, error) {
		if err := ExpectText(content, index, taskText); err != nil {
			return "", err
		}
		for _, it := range domain.ParseChecklist(content) {
			if it.Index == index && it.Done {
				// Done covers [x] and [-] alike: either way it is not a
				// pending task waiting to be handed out.
				return "", fmt.Errorf("task #%d is no longer pending — refresh and retry", index)
			}
		}
		return domain.MarkChecklistItemInProgress(content, index)
	}
}

// Release undoes a reservation after a failed delivery, returning the item to
// [ ]. It is claim-scoped: it only resets an item that still carries this
// reservation's text AND is still [-]. Resetting on text alone would let a
// rollback silently re-open work somebody else completed in the meantime — and
// re-arm it for the daemon. Anything else is left [-], which merely parks the
// task rather than risking a second delivery.
func Release(index int, taskText string) func(string) (string, error) {
	return func(content string) (string, error) {
		if err := ExpectText(content, index, taskText); err != nil {
			return "", err
		}
		for _, it := range domain.ParseChecklist(content) {
			if it.Index == index && it.Mark != domain.MarkInProgress {
				return "", fmt.Errorf("task #%d is now [%s], not the [-] this send reserved", index, it.Mark)
			}
		}
		return domain.SetChecklistItemDone(content, index, false)
	}
}

// ReserveFirstPending claims the FIRST still-pending "[ ]" item whose text
// equals taskText, marking it [-], and reports which index it claimed via the
// returned pointer (valid only after a successful Mutate).
//
// It exists for the daemon's auto-send path, where the index resolved when the
// situation was captured can be stale by delivery time: an LLM pre-send review
// takes seconds and may itself pick a different pending task, and the operator
// can edit the list meanwhile. Locating the item by text inside the lock keeps
// the claim atomic without threading a fragile index through the async
// pipeline. A task text that is no longer pending is an error — that is
// exactly the "somebody else already took it" case the caller must not send.
func ReserveFirstPending(taskText string) (func(string) (string, error), *int) {
	claimed := new(int)
	*claimed = -1
	return func(content string) (string, error) {
		for _, it := range domain.ParseChecklist(content) {
			if it.Done || it.Text != taskText {
				continue
			}
			out, err := domain.MarkChecklistItemInProgress(content, it.Index)
			if err != nil {
				return "", err
			}
			*claimed = it.Index
			return out, nil
		}
		return "", fmt.Errorf("no pending task matching %q remains in the list — it was completed, claimed or edited", taskText)
	}, claimed
}

// Reclaim returns one "[-]" item carrying taskText to "[ ]", so the next idle
// sweep can hand it out again. It is the daemon's counterpart to
// ReserveFirstPending, undoing a hand-out the agent never took up.
//
// It prefers the position the reservation recorded and falls back to the first
// text match, because neither key alone is sound here: positions renumber on
// every insert or delete, so an index resolved minutes ago can address a
// different line; but text is not unique either — a checklist may well repeat
// "run tests", and clearing the FIRST such "[-]" when the reservation claimed
// the second would take back an item somebody else owns. Preferring the index
// while it still carries the reserved text gets both. Pass index <= 0 when
// none was recorded (rows written before the column existed).
//
// Deliberately narrow, because the caller is unattended:
//   - only a "[-]" item is touched. An item now "[x]" was completed and an item
//     already "[ ]" needs nothing — either way, silently re-opening it would
//     re-arm work somebody finished.
//   - "no such item" is an error, not a no-op, so the caller can tell "left it
//     alone" from "reclaimed it" and audit accordingly.
//
// The caller must additionally prove the "[-]" is the daemon's OWN reservation
// (a task_reservations row); this helper cannot distinguish an operator's or an
// agent's own in-progress mark from HAP's.
func Reclaim(index int, taskText string) func(string) (string, error) {
	return func(content string) (string, error) {
		items := domain.ParseChecklist(content)
		for _, it := range items {
			if it.Index == index && it.Text == taskText && it.Mark == domain.MarkInProgress {
				return domain.SetChecklistItemDone(content, it.Index, false)
			}
		}
		for _, it := range items {
			if it.Text == taskText && it.Mark == domain.MarkInProgress {
				return domain.SetChecklistItemDone(content, it.Index, false)
			}
		}
		return "", fmt.Errorf("no in-progress task matching %q remains in the list — it was completed, released or edited", taskText)
	}
}
