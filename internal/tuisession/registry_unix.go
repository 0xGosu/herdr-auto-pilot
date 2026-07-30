//go:build !windows

package tuisession

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
)

// errUnsupported reports a platform without the file locks this package needs,
// so Register stops retrying instead of sleeping through every attempt.
var errUnsupported = errors.New("the TUI instance limit needs file locks")

// claim opens path and takes its exclusive lock, which the session then holds
// for its whole run. An already-locked file means a live peer with our pid —
// impossible in practice — or, far more likely, a peer briefly probing the
// file; either way the caller retries.
func claim(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	// Drop the previous holder's bytes the moment the lock is ours. Waiting
	// for write() would leave a window in which this pid's record still shows
	// a DEAD predecessor's start time — and a peer reading it there would take
	// the newest TUI for the oldest one and close it.
	if err := f.Truncate(0); err != nil {
		release(f)
		return nil, fmt.Errorf("clear %s: %w", path, err)
	}
	return f, nil
}

// processIdentity names the process space this pid belongs to, so a state dir
// shared between a container and its host (a bind-mounted home) does not let
// one side signal a pid that means something else on the other. Unknown ("")
// disables the comparison rather than the limit.
//
// BOTH parts are needed, so it is hostname + pid namespace whenever both are
// readable. The namespace alone does not identify a machine: the initial pid
// namespace has a fixed inode, so every uncontainerized Linux host reports the
// same string — and two hosts sharing a state dir over a network home would
// then read as one, with flock visible across clients and a signal landing on
// whatever local process happens to hold that pid. The hostname alone misses
// the container-and-its-host case. Their failure modes are asymmetric: a
// wrongly-foreign peer only turns the limit off, while a wrongly-local one
// signals a stranger, so a hostname that changes under us (a laptop taking a
// new DHCP name) is the cost worth paying.
func processIdentity() string {
	host, hostErr := os.Hostname()
	ns, nsErr := os.Readlink("/proc/self/ns/pid")
	if hostErr != nil {
		host = ""
	}
	if nsErr != nil {
		ns = ""
	}
	return sanitizeIdentity(strings.TrimSpace(host + " " + ns))
}

// sanitizeIdentity keeps an identity to the one line the record format allows;
// a newline would forge an extra field, and there is no legitimate one.
func sanitizeIdentity(id string) string {
	id = strings.TrimSpace(id)
	if strings.ContainsAny(id, "\r\n") {
		return ""
	}
	return id
}

// release drops the lock and closes the file.
func release(f *os.File) {
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	f.Close()
}

// held reports whether some process still holds path's lock. It never creates
// the file: a record that vanished between the directory listing and this call
// is simply not live.
func held(path string) (bool, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return false, err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		// EWOULDBLOCK is the answer we came for; anything else is a real
		// failure to probe.
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return true, nil
		}
		return false, err
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false, nil
}

// signalStop asks a TUI to exit the way closing its pane does: SIGTERM cancels
// its run context, so it restores the terminal and closes its store instead of
// dying where it stands (cmd/hap handles it in shutdownSignals). A pid that is
// already gone is not an error.
func signalStop(pid int) error {
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}
