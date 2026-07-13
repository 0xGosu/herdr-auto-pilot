package daemonhealth

import (
	"os"
	"path/filepath"
)

// StderrLogName is the basename of the captured daemon stderr log. The
// detached daemon's stderr is redirected here so a native abort in the
// embedder (llama.cpp GGML_ASSERT → SIGABRT) — invisible to Go recovery —
// leaves a post-mortem trail instead of going to /dev/null.
const StderrLogName = "daemon.stderr.log"

// StderrLogCap bounds the captured stderr log. Past it the file is rotated to
// a single ".old" sibling on the next open, so a crash-loop cannot fill the
// disk while the most recent abort is always retained.
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
