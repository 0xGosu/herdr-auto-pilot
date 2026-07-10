//go:build windows

package daemonlock

import (
	"errors"
	"os/exec"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
)

// Windows support is a follow-up (NFR-008); these stubs keep the build
// portable without locking the daemon path to Unix APIs.

// Lock is a stub; the daemon cannot run on Windows yet.
type Lock struct{}

// Acquire always fails on Windows.
func Acquire(paths config.Paths) (*Lock, error) {
	return nil, errors.New("the daemon is not yet supported on Windows (MVP targets Linux/macOS)")
}

// Release is a no-op stub.
func (l *Lock) Release() {}

// Info always reports no running daemon, so EnsureFresh only ever calls
// start (which then fails in Acquire with the message above).
func Info(paths config.Paths) (running bool, pid int, version string) { return false, 0, "" }

// Stop is unreachable on Windows (Info never reports a holder).
func Stop(pid int) error {
	return errors.New("the daemon is not yet supported on Windows")
}

// Detach is a no-op stub.
func Detach(cmd *exec.Cmd) {}
