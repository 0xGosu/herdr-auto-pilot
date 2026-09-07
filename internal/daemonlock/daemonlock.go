// Package daemonlock guards the single monitoring daemon per state
// directory with a file lock, and records the holder's pid and version so
// `hap daemon --ensure` can replace a daemon left running from an older
// binary (upgrading the binary on disk never restarts the live process).
package daemonlock

import (
	"fmt"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/selfpath"
)

// EnsureFresh implements `hap daemon --ensure`: start a daemon when none is
// running, no-op when the running one matches this binary, and gracefully
// replace one left behind by a different binary. stop asks a pid to shut
// down (SIGTERM); start launches a detached daemon. waitTimeout bounds how
// long a stale daemon gets to release the lock after stop.
//
// exePath is this binary's path; a holder running from a DIFFERENT one is
// stale even at the same version, because an upgrade that reuses the version
// string still unlinks the old binary and strands every child the running
// daemon spawns from it. Pass "" to compare on version alone.
func EnsureFresh(paths config.Paths, version, exePath string, waitTimeout time.Duration, stop func(pid int) error, start func() error) error {
	_, err := replaceHolder(paths, version, exePath, waitTimeout, stop, start, false)
	return err
}

// Outcome describes what a Restart did, so the caller can report it without a
// second Info() read — which would race the daemon it just started and could
// name the WRONG pid, or claim nothing had been running when something had.
type Outcome struct {
	// Replaced reports that a running daemon was stopped before the start.
	Replaced bool
	// StoppedPID and StoppedVersion describe that daemon; zero when none ran.
	StoppedPID     int
	StoppedVersion string
}

// Restart implements `hap daemon --restart`: stop whatever daemon is running —
// current binary or not — and start a fresh one, starting one when none is
// running. It shares EnsureFresh's body rather than reimplementing it because
// every step below the version comparison is load-bearing (the pid re-probe
// over the lock-rewrite window, and WaitReleased before start, without which a
// restart races the outgoing daemon for the lock and the new one dies).
//
// A restart exists for what a reload cannot do: [database] and [logging] are
// read once, when a process opens its store, so switching engines — or pointing
// at a different Turso database — takes effect only in a NEW process.
func Restart(paths config.Paths, waitTimeout time.Duration, stop func(pid int) error, start func() error) (Outcome, error) {
	return replaceHolder(paths, "", "", waitTimeout, stop, start, true)
}

// replaceHolder is EnsureFresh's body, with force skipping only the "the
// running holder is already the binary we would start" early return.
func replaceHolder(paths config.Paths, version, exePath string, waitTimeout time.Duration, stop func(pid int) error, start func() error, force bool) (Outcome, error) {
	running, pid, ver, exe := info(paths)
	if !running {
		return Outcome{}, start()
	}
	if !force && current(ver, exe, version, exePath) {
		return Outcome{}, nil
	}
	if pid <= 0 {
		// A fresh holder truncates then rewrites the lock file; one
		// re-probe skips that microsecond window before giving up.
		time.Sleep(100 * time.Millisecond)
		if running, pid, ver, exe = info(paths); !running {
			return Outcome{}, start()
		}
		if !force && current(ver, exe, version, exePath) {
			return Outcome{}, nil
		}
	}
	if pid <= 0 {
		return Outcome{}, fmt.Errorf("daemon lock held but pid unreadable; stop it manually (pkill -f 'hap daemon')")
	}
	if err := stop(pid); err != nil {
		return Outcome{}, fmt.Errorf("stop stale daemon (pid %d, %s): %w", pid, VersionLabel(ver), err)
	}
	if !WaitReleased(paths, waitTimeout) {
		return Outcome{}, fmt.Errorf("stale daemon (pid %d, %s) did not exit within %s; stop it manually (pkill -f 'hap daemon')",
			pid, VersionLabel(ver), waitTimeout)
	}
	return Outcome{Replaced: true, StoppedPID: pid, StoppedVersion: ver}, start()
}

// current reports whether the running holder is the binary we would start.
// The path check only applies when BOTH sides are known and resolvable: a
// pre-path lock file records none, and an unresolvable path would otherwise
// make every --ensure kill a perfectly good daemon. Paths are compared
// through symlinks so invoking via /usr/local/bin/hap does not read as a
// different install than <plugin>/bin/hap.
func current(holderVersion, holderExe, version, exePath string) bool {
	if holderVersion != version {
		return false
	}
	if holderExe == "" || exePath == "" {
		return true
	}
	return selfpath.Same(holderExe, exePath)
}

// WaitReleased polls until no daemon holds the lock or the timeout elapses.
func WaitReleased(paths config.Paths, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if running, _, _ := Info(paths); !running {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// VersionLabel renders a lock-file version for messages; daemons older than
// the pid+version lock format recorded none.
func VersionLabel(v string) string {
	if v == "" {
		return "unversioned, older binary"
	}
	return v
}
