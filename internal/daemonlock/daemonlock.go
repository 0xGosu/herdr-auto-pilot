// Package daemonlock guards the single monitoring daemon per state
// directory with a file lock, and records the holder's pid and version so
// `hap daemon --ensure` can replace a daemon left running from an older
// binary (upgrading the binary on disk never restarts the live process).
package daemonlock

import (
	"fmt"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
)

// EnsureFresh implements `hap daemon --ensure`: start a daemon when none is
// running, no-op when the running one matches version, and gracefully
// replace one left behind by a different binary. stop asks a pid to shut
// down (SIGTERM); start launches a detached daemon. waitTimeout bounds how
// long a stale daemon gets to release the lock after stop.
func EnsureFresh(paths config.Paths, version string, waitTimeout time.Duration, stop func(pid int) error, start func() error) error {
	running, pid, ver := Info(paths)
	if !running {
		return start()
	}
	if ver == version {
		return nil
	}
	if pid <= 0 {
		// A fresh holder truncates then rewrites the lock file; one
		// re-probe skips that microsecond window before giving up.
		time.Sleep(100 * time.Millisecond)
		if running, pid, ver = Info(paths); !running {
			return start()
		}
		if ver == version {
			return nil
		}
	}
	if pid <= 0 {
		return fmt.Errorf("daemon lock held but pid unreadable; stop it manually (pkill -f 'hap daemon')")
	}
	if err := stop(pid); err != nil {
		return fmt.Errorf("stop stale daemon (pid %d, %s): %w", pid, VersionLabel(ver), err)
	}
	if !WaitReleased(paths, waitTimeout) {
		return fmt.Errorf("stale daemon (pid %d, %s) did not exit within %s; stop it manually (pkill -f 'hap daemon')",
			pid, VersionLabel(ver), waitTimeout)
	}
	return start()
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
