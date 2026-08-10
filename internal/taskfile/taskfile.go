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

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/tasklocator"
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
	// Expand ~/$VAR here (and in LockPath below) so every process mutating a
	// task_sources.path — daemon, CLI, TUI — reads, writes, and LOCKS the same
	// physical file regardless of which shorthand the config used.
	path = config.ExpandPath(path)
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

// LockPath returns a stable, hap-owned lock-file path for a task list, keyed by
// its canonical locator. Keeping the lock in a shared temp dir — rather than a
// `<file>.lock` sidecar — serializes concurrent mutations without dropping a
// stray lock file into the user's repo next to a --path checklist.
//
// The locator is canonicalized by tasklocator.Canonical (~/$VAR expanded, then
// absolute + symlinks resolved for a filesystem path; returned verbatim for a
// scheme'd one) so every caller — the daemon, the CLI, the TUI's add/edit, and
// the TUI's bulk toggle/delete, which passes an already symlink-resolved path —
// hashes to the SAME key for one list. Without this, a symlinked path component
// (e.g. macOS /var vs /private/var), or one config spelling a source
// `~/tasks.md` and another its absolute form, would yield two different locks
// and stop serializing concurrent mutations of the same checklist.
//
// The lock is what serializes hap's processes over a REMOTE list too: a gist
// has no compare-and-swap, so its read-modify-write is guarded by exactly this
// lock rather than by a conditional write.
func LockPath(locator string) string {
	sum := sha256.Sum256([]byte(tasklocator.Canonical(locator)))
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

// ReviewOutcome reports what a pre-delivery task-list review actually did to
// the checklist. It is filled by the mutator ApplyReview returns and is valid
// only after that mutator ran to completion inside a successful Mutate.
type ReviewOutcome struct {
	// Applied records every operation in order, with before/after text, for
	// the audit row. An operator must be able to answer "why is task 4 gone?"
	// from this alone.
	Applied []domain.AppliedTaskAction
	// SentIndex / SentText address the task the review chose to deliver, in
	// the FINAL list. Both are zero/empty for a noop.
	SentIndex int
	SentText  string
	// SentFolded is that task's delivery body — its text plus the detail
	// folded under it — captured from the very snapshot the safety gates
	// inspected and the reservation was written against. The caller MUST
	// render its outbound prompt from this rather than re-reading the file: a
	// second read could fold a different block than the one that was screened,
	// which is exactly the send-text/scan/audit divergence FR-015 forbids.
	SentFolded string
	// Reserved is true when the chosen task was marked "[-]" as part of this
	// same critical section, so the caller knows to record a ledger row after
	// the send and to skip the ordinary reserve step.
	Reserved bool
	// Noop is true when the review declined to send anything. It is accepted
	// only for a genuinely exhausted source, which is checked here.
	Noop bool
}

// ApplyReview is the single critical section a pre-delivery LLM review commits
// through: it validates every reference, applies every action, resolves the
// task to send, re-screens that task's delivery text through the caller's
// safety gates, and reserves it — all inside ONE locked read-modify-write.
//
// Doing it in one pass is a correctness requirement, not tidiness. A
// reservation is written by INDEX and the ledger row carries that index, so an
// index captured before the mutations points at the wrong task afterwards. The
// order here — mutate, then re-resolve, then reserve — is what makes the
// recorded position address the item that was actually delivered.
//
// It is also strictly ALL-OR-NOTHING, and structurally so: every failure path
// returns an error, and MutateWithin writes nothing when the mutator errors.
// The checklist is either fully updated or byte-identical. A review the
// operator's confidence threshold rejects, or one naming a task that no longer
// exists, can never half-edit their file.
//
//   - expectIndex/expectText guard against a checklist that changed while the
//     consult ran (an LLM CLI takes seconds): the reviewed task must still be
//     exactly where and what it was, or the whole submission is refused rather
//     than applied to a list it was not written against. Pass expectIndex <= 0
//     to skip the guard.
//   - safe re-screens the FINAL folded delivery text — never-auto patterns and
//     the suspected-irreversible heuristic (FR-015). The reviewer is an LLM
//     authoring both task text and the choice of task, so its output is gated
//     exactly like any other LLM-authored outbound. It runs BEFORE the write,
//     so a trip leaves the file untouched and the caller falls back to the
//     original task.
//   - reserve mirrors declared.Reserve: only an auto-sending source marks its
//     delivered item "[-]".
//
// maxTasks caps the list size an add may grow to (0 = uncapped).
func ApplyReview(expectIndex int, expectText, sendRef string, actions []domain.TaskAction,
	maxTasks int, reserve bool, safe func(folded string) error) (func(string) (string, error), *ReviewOutcome) {

	out := &ReviewOutcome{}
	return func(content string) (string, error) {
		// Built locally and published to *out only on the success path, so a
		// caller reading the outcome after an error can never see a delivery
		// that did not happen. The all-or-nothing rule covers what the caller
		// is TOLD as much as what the file holds: a half-filled outcome would
		// have the daemon audit — or reserve against — a task it never sent.
		var got ReviewOutcome

		// 1. The list must still be the one the review was shown.
		if expectIndex > 0 {
			if err := ExpectText(content, expectIndex, expectText); err != nil {
				return "", err
			}
		}

		// 2. Apply the whole series, or none of it.
		res, err := domain.ApplyTaskActions(content, actions, maxTasks)
		if err != nil {
			return "", err
		}
		got.Applied = res.Applied

		// 3. Resolve the chosen task against the FINAL list.
		items := domain.ParseChecklist(res.Content)
		chosen, err := domain.ResolveSendTask(items, sendRef, res.Handles)
		if err != nil {
			return "", err
		}
		if chosen.Noop {
			// Declining is legal only when the source is genuinely dry. The
			// review exists to keep an agent working, so "nothing to send"
			// with pending work left is a malformed submission, not a policy.
			if domain.HasPendingTask(items) {
				return "", fmt.Errorf("send_task is %q but pending tasks remain — a review may only decline an exhausted source",
					domain.NoopSendTask)
			}
			got.Noop = true
			*out = got
			return res.Content, nil
		}
		got.SentIndex, got.SentText = chosen.Index, chosen.Text
		// Fold ONCE, from this snapshot: the same bytes are screened below,
		// delivered by the caller, and recorded on the audit row.
		got.SentFolded = domain.FoldTaskContentAt(res.Content, chosen.Index)
		if got.SentFolded == "" {
			got.SentFolded = chosen.Text
		}

		// 4. Re-gate the LLM-authored delivery text before anything is written.
		if safe != nil {
			if err := safe(got.SentFolded); err != nil {
				return "", err
			}
		}

		// 5. Claim the item, now that its position is final.
		if !reserve {
			*out = got
			return res.Content, nil
		}
		claimed, err := domain.MarkChecklistItemInProgress(res.Content, chosen.Index)
		if err != nil {
			return "", err
		}
		got.Reserved = true
		*out = got
		return claimed, nil
	}, out
}

// Reclaim returns one "[-]" item carrying taskText to "[ ]", so the next idle
// sweep can hand it out again. It is the daemon's counterpart to
// ReserveFirstPending, undoing a hand-out the agent never took up.
//
// Neither of the two available keys is sound alone: positions renumber on every
// insert or delete, so an index resolved minutes ago can address a different
// line; but text is not unique either — a checklist may well repeat "run tests".
// So it resolves in two steps, and the second one FAILS CLOSED:
//
//  1. the recorded position, while it still carries the reserved text as "[-]" —
//     unambiguous, and the normal case;
//  2. otherwise the sole "[-]" carrying that text, but ONLY when the text
//     appears once in the whole checklist. A duplicated text means the caller
//     cannot prove which copy its reservation claimed, and clearing the wrong one
//     would take back an item somebody else owns — precisely the invariant this
//     is meant to uphold. Refusing leaves the item "[-]", which is where the
//     feature stood before any of this existed.
//
// Pass index <= 0 when none was recorded (rows written before the position was
// stored); step 2 then decides on its own, under the same uniqueness rule.
//
// Deliberately narrow otherwise, because the caller is unattended:
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
		occurrences, inProgress := 0, -1
		for _, it := range items {
			if it.Text != taskText {
				continue
			}
			occurrences++
			if it.Mark != domain.MarkInProgress {
				continue
			}
			if it.Index == index {
				return domain.SetChecklistItemDone(content, it.Index, false)
			}
			inProgress = it.Index
		}
		if inProgress < 0 {
			return "", fmt.Errorf("no in-progress task matching %q remains in the list — it was completed, released or edited", taskText)
		}
		if occurrences > 1 {
			return "", fmt.Errorf("task #%d is not the %q this send reserved and the text appears %d times — "+
				"refusing to release a copy that may not be ours", index, taskText, occurrences)
		}
		return domain.SetChecklistItemDone(content, inProgress, false)
	}
}
