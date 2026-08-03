package daemonhealth

import (
	"io"
	"os"
	"path/filepath"
)

// StderrLogName is the basename of the captured daemon stderr log. The
// detached daemon's stderr is redirected here so a native abort in the
// embedder (llama.cpp GGML_ASSERT → SIGABRT) — invisible to Go recovery —
// leaves a post-mortem trail instead of going to /dev/null.
const StderrLogName = "daemon.stderr.log"

// StderrLogCap bounds the captured stderr log. Past it the file is rotated to
// a single ".old" sibling, so a crash-loop cannot fill the disk while the most
// recent abort is always retained.
const StderrLogCap = 256 * 1024

// StderrLogPath returns the captured stderr log path for a state directory.
func StderrLogPath(stateDir string) string {
	return filepath.Join(stateDir, StderrLogName)
}

// OpenStderrLog opens the daemon stderr log for appending, first rotating it to
// a ".old" sibling if it has grown past StderrLogCap. The returned file is
// meant to be handed to exec.Cmd.Stderr; the caller closes its copy after the
// child starts (the child dup'd the fd). Returns nil on any error — capture is
// a best-effort diagnostic aid, never a reason to fail the daemon spawn.
func OpenStderrLog(stateDir string) *os.File {
	p := StderrLogPath(stateDir)
	if fi, err := os.Stat(p); err == nil && fi.Size() > StderrLogCap {
		_ = os.Rename(p, p+".old")
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil
	}
	return f
}

// RotateOwnStderrIfNeeded bounds the stderr log a RUNNING daemon is writing to,
// and is what makes the cap hold for a process that never restarts.
//
// OpenStderrLog can only check at spawn time, so a daemon that runs for weeks
// never rotates however much it writes — the same flaw that made the plugin log
// reach 1.9 GB (see logging.rotatingFile, which fixed it by checking on write).
// This cannot use that fix: the daemon is DETACHED, so the parent is gone and
// nothing can wrap the child's fd 2 in a Go writer. Native aborts write straight
// to the descriptor, which is the entire point of the capture.
//
// So it rotates in place instead: copy the file aside as ".old", then TRUNCATE
// the live one. Truncation, not rename, because the daemon's fd 2 is already
// bound to this inode — a rename would leave it writing into the ".old" file
// while the fresh one stayed empty forever. The descriptor is O_APPEND, so the
// kernel re-seeks to the (now zero) end on the next write and no sparse gap
// appears.
//
// Two bounds are accepted rather than solved, both because the alternative is
// worse than the failure:
//
//   - Anything written between the copy and the truncate is discarded. fd 2 is
//     shared with the embed worker, so a GGML_ASSERT landing in that window is
//     lost. The window is microseconds and only opens once the log is already
//     past the cap; holding a lock across a native abort is not possible.
//   - Only the last StderrLogCap bytes are carried into ".old". A crash trail
//     is a tail, and slurping a file that is large BY DEFINITION — on a
//     rotation path that exists for the disk-full case — is the wrong shape.
//
// Every failure is swallowed: this is a diagnostic file, and a failed rotation
// degrades to "it keeps growing", never to a lost daemon.
func RotateOwnStderrIfNeeded(stateDir string) {
	fi, err := os.Stderr.Stat()
	// A foreground daemon's stderr is a terminal, and a test's may be a pipe.
	// Only a regular file that has grown past the cap is ours to rotate.
	if err != nil || !fi.Mode().IsRegular() || fi.Size() <= StderrLogCap {
		return
	}
	p := StderrLogPath(stateDir)
	// Rotate only what we are actually attached to. If the path now names a
	// different inode, another process already rotated it and those bytes are
	// not ours to copy over its ".old".
	onDisk, err := os.Stat(p)
	if err != nil || !os.SameFile(fi, onDisk) {
		return
	}
	if !copyTail(p, p+".old", StderrLogCap) {
		return
	}
	_ = os.Stderr.Truncate(0)
}

// copyTail writes the last n bytes of src to dst, reporting success. The whole
// point is to bound the copy: the caller runs on an oversized file, and on the
// heartbeat, so reading it entirely would scale with the problem it is fixing.
func copyTail(src, dst string, n int64) bool {
	in, err := os.Open(src)
	if err != nil {
		return false
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return false
	}
	if fi.Size() > n {
		if _, err := in.Seek(fi.Size()-n, io.SeekStart); err != nil {
			return false
		}
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return false
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return false
	}
	return out.Close() == nil
}
