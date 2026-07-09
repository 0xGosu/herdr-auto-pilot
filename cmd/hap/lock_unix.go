//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
)

// daemonLock is a flock-based singleton guard so only one daemon monitors
// the herd per state directory.
type daemonLock struct {
	f *os.File
}

func lockPath(paths config.Paths) string {
	return filepath.Join(paths.StateDir, "daemon.lock")
}

func acquireDaemonLock(paths config.Paths) (*daemonLock, error) {
	f, err := os.OpenFile(lockPath(paths), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another daemon already holds the lock: %w", err)
	}
	fmt.Fprintf(f, "%d\n", os.Getpid())
	return &daemonLock{f: f}, nil
}

func (l *daemonLock) release() {
	if l.f != nil {
		syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
		l.f.Close()
	}
}

// daemonRunning reports whether a daemon currently holds the lock.
func daemonRunning(paths config.Paths) bool {
	f, err := os.OpenFile(lockPath(paths), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return true // lock held → daemon running
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

// detach configures cmd to run in its own session, surviving hook exit.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
