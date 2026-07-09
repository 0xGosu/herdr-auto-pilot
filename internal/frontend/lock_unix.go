//go:build !windows

package frontend

import (
	"os"
	"syscall"
)

// lockFile takes an advisory exclusive lock on path (creating it if needed)
// and returns the unlock function. Serializes config read-modify-write
// across concurrent front-end processes.
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
