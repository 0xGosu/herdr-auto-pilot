package logging

import (
	"os"
	"sync"
)

// LogCap bounds the plugin log. Past it the file is rotated to a single ".old"
// sibling, keeping roughly 2*LogCap on disk instead of the unbounded growth
// that took a live 0.5.19 state directory to 1.9 GB.
//
// The cap is enforced ON WRITE, not only on open. Every hap process appends to
// this one file, but the daemon holds it open for its whole life — so an
// open-time check alone (what the captured stderr log does) never fires for the
// process that produces nearly all the volume.
const LogCap = 64 << 20 // 64 MiB

// rotatingFile is an io.Writer over an append-mode log file that rotates itself
// once it passes LogCap.
//
// Size is tracked locally rather than stat'd per write (one syscall per log
// line would be a real cost on the daemon path), and re-synced from the file
// whenever the threshold is crossed. Concurrent hap processes each hold their
// own handle: the loser of a rotation race notices its handle no longer names
// the live path and simply reopens, so a rotation by any process is picked up
// by the rest instead of silently splitting the log across two inodes.
//
// Two bounds are approximate BY DESIGN, because the alternative is a syscall or
// a lock per log line for a diagnostic file:
//   - The counter only sees THIS process's writes since its own open, so the
//     live file can overshoot LogCap by whatever other hap processes appended
//     in the meantime. They are short-lived and write little; the daemon, which
//     writes nearly everything, is the one holding an accurate count.
//   - The SameFile check below narrows but does not close a rotation race: two
//     processes can both pass it and the second rename can drop one file's
//     worth of history. Losing old log lines is the acceptable failure here;
//     unbounded growth was not.
type rotatingFile struct {
	path string
	max  int64

	mu   sync.Mutex
	f    *os.File
	size int64
}

func newRotatingFile(path string, max int64) (*rotatingFile, error) {
	r := &rotatingFile{path: path, max: max}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

// open attaches to the live log file and records its current size. It leaves
// r.f untouched on failure, so the caller keeps whatever handle it had.
func (r *rotatingFile) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	// Start from the file's real size, not zero: attaching to an already-large
	// log (the daemon restarting over one an earlier run left behind) must
	// rotate on the next write, not after another full LogCap.
	size := int64(0)
	if fi, err := f.Stat(); err == nil {
		size = fi.Size()
	} else if fi, err := os.Stat(r.path); err == nil {
		size = fi.Size()
	}
	r.f, r.size = f, size
	return nil
}

func (r *rotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size+int64(len(p)) > r.max {
		r.rotate()
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// rotate renames the log to its ".old" sibling and reopens. Every failure is
// swallowed on purpose: logging must never take down the daemon, and a failed
// rotation degrades to "the log keeps growing", not to a lost log line — the
// caller writes to whatever handle is left.
func (r *rotatingFile) rotate() {
	fi, statErr := os.Stat(r.path)
	ours, ourErr := r.f.Stat()
	// Rename only what we are still attached to. If another process already
	// rotated (or removed) the file underneath us, those bytes are not ours to
	// move — renaming would clobber the ".old" that process just created. Then
	// reopen either way, which is what reattaches us to the live path.
	if statErr == nil && ourErr == nil && os.SameFile(fi, ours) {
		_ = os.Rename(r.path, r.path+".old")
	}
	old := r.f
	if err := r.open(); err != nil {
		// Reopen failed, so open() left us on the handle we already had — which
		// now points at the rotated inode. Keep the REAL size rather than
		// resetting it: a zeroed counter would let that abandoned file grow
		// another full cap before anything retried, and the rotation that
		// eventually succeeded would rename the recreated live file over
		// ".old", destroying everything written in between. Leaving the count
		// where it is makes the very next write retry the rotation.
		if fi, statErr := old.Stat(); statErr == nil {
			r.size = fi.Size()
		}
		return
	}
	_ = old.Close()
}
