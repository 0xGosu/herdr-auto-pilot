//go:build windows

package main

import (
	"errors"
	"os/exec"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
)

// Windows support is a follow-up (NFR-008); these stubs keep the build
// portable without locking the daemon path to Unix APIs.

type daemonLock struct{}

func acquireDaemonLock(paths config.Paths) (*daemonLock, error) {
	return nil, errors.New("the daemon is not yet supported on Windows (MVP targets Linux/macOS)")
}

func (l *daemonLock) release() {}

func daemonRunning(paths config.Paths) bool { return false }

func detach(cmd *exec.Cmd) {}
