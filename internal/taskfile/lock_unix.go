//go:build !windows

package taskfile

import (
	"fmt"
	"os"
	"syscall"
	"time"
)

// Lock takes an advisory exclusive lock on path (creating it if needed) and
// returns the unlock function. It serializes read-modify-write cycles across
// concurrent hap processes — the daemon, the CLI and the TUI all mutate the
// same checklist (and config) files. It blocks until the lock is available.
func Lock(path string) (func(), error) {
	return lock(path, 0)
}

// LockWithin is Lock bounded by a deadline: it polls for the lock and gives up
// with an error once wait has elapsed. The daemon uses it because its
// reserve-before-send runs on the main select loop — an unbounded wait behind
// another process's lock (a wedged `hap task`, a slow network-backed
// checklist) would stall every agent, not just this one. Failing to take the
// lock resolves to "do not send", which is the safe outcome.
func LockWithin(path string, wait time.Duration) (func(), error) {
	return lock(path, wait)
}

// lockPollInterval is how often a bounded wait retries the non-blocking lock.
const lockPollInterval = 20 * time.Millisecond

func lock(path string, wait time.Duration) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	unlock := func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}
	if wait <= 0 {
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
			f.Close()
			return nil, err
		}
		return unlock, nil
	}
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return unlock, nil
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			f.Close()
			return nil, err
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("task file is locked by another hap process (waited %s)", wait)
		}
		time.Sleep(lockPollInterval)
	}
}
