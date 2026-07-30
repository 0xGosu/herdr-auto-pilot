//go:build !windows

package tuisession

import (
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
)

// signalStop maps "already gone" to success: a peer that exited between the
// sweep that listed it and the signal is exactly the outcome we wanted, and
// treating it as a failure would log a warning on the happy path.
func TestSignalStopOnAVanishedProcess(t *testing.T) {
	// A pid no process can hold: above the kernel's pid_max by construction.
	if err := signalStop(1 << 30); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			t.Fatal("ESRCH reached the caller; it must be swallowed")
		}
		t.Fatalf("signalStop on a dead pid = %v, want nil", err)
	}
}

// processIdentity never returns something that could forge an extra line in a
// session record.
func TestProcessIdentityIsSingleLine(t *testing.T) {
	for _, r := range processIdentity() {
		if r == '\n' || r == '\r' {
			t.Fatalf("processIdentity() = %q contains a newline", processIdentity())
		}
	}
}

// The identity must carry the HOSTNAME, not the pid namespace alone: the
// initial namespace has a fixed inode, so every uncontainerized Linux host
// reports the same one — and two hosts sharing a state dir over a network home
// would then read as one process space and signal each other's pids.
func TestProcessIdentityDistinguishesHosts(t *testing.T) {
	host, err := os.Hostname()
	if err != nil || host == "" {
		t.Skip("this platform reports no hostname")
	}
	id := processIdentity()
	if !strings.Contains(id, host) {
		t.Errorf("processIdentity() = %q, want it to include the hostname %q", id, host)
	}
	if ns, err := os.Readlink("/proc/self/ns/pid"); err == nil && ns != "" && !strings.Contains(id, ns) {
		t.Errorf("processIdentity() = %q, want it to include the pid namespace %q too", id, ns)
	}
}
