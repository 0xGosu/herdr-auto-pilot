//go:build !windows

package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/buildinfo"
	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/selfpath"
)

// handOff records what deferToRegisteredInstall did with its two seams.
type handOff struct {
	asked int      // registry lookups
	bin   string   // binary handed to
	args  []string // its arguments
	env   []string
}

// stubSupersession describes an install layout: registered is what herdr's
// registry names ("" = this binary is still the installed one), and runErr is
// what running it returns. Nothing is ever executed.
func stubSupersession(t *testing.T, registered string, runErr error) *handOff {
	t.Helper()
	origAsk, origRun := supersededBy, runRegistered
	t.Cleanup(func() { supersededBy, runRegistered = origAsk, origRun })
	h := &handOff{}
	supersededBy = func(string) string {
		h.asked++
		return registered
	}
	runRegistered = func(bin string, env []string, args ...string) error {
		h.bin, h.env, h.args = bin, env, args
		return runErr
	}
	return h
}

func supersedePaths(t *testing.T) config.Paths {
	t.Helper()
	t.Setenv(delegatedEnv, "")
	return config.Paths{ConfigDir: t.TempDir(), StateDir: t.TempDir()}
}

const newInstall = "/plugins/github-installations/herd-auto-prompter-abc/2-0/checkout/bin/hap"

// TestEnsureFromASupersededInstallDefersToTheRegisteredOne is the herdr 0.9.5
// regression: a replaced installation now stays on disk, so `hap daemon
// --ensure` typed through a PATH link that still points at it is a working OLD
// build. It must not start itself (here: with no daemon running — the replace
// case is the same decision, and staging it would signal a real pid).
func TestEnsureFromASupersededInstallDefersToTheRegisteredOne(t *testing.T) {
	paths := supersedePaths(t)
	h := stubSupersession(t, newInstall, nil)

	if err := ensureDaemon(paths, false); err != nil {
		t.Fatalf("ensureDaemon: %v", err)
	}
	if h.bin != newInstall || !slices.Equal(h.args, []string{"daemon", "--ensure"}) {
		t.Fatalf("handed to %q %v, want %q [daemon --ensure]", h.bin, h.args, newInstall)
	}
	// The mark that stops the registered binary deferring again.
	if !slices.Contains(h.env, delegatedEnv+"=1") {
		t.Errorf("the handed-off run does not carry %s", delegatedEnv)
	}
}

// The hot path: herdr's hooks run --ensure on every agent and workspace event
// and almost always find the daemon current. That must not cost a registry
// lookup — nor anything else.
func TestEnsureAsksNothingWhenTheDaemonIsAlreadyCurrent(t *testing.T) {
	paths := supersedePaths(t)
	self, err := selfpath.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	holdDaemonLock(t, paths, fmt.Sprintf("%d\n%s\n%s\n", os.Getpid(), buildinfo.Version, self))
	h := stubSupersession(t, newInstall, nil)

	if err := ensureDaemon(paths, false); err != nil {
		t.Fatalf("ensureDaemon: %v", err)
	}
	if h.asked != 0 || h.bin != "" {
		t.Errorf("asked the registry %d time(s), handed to %q; want neither", h.asked, h.bin)
	}
}

// --replace-only is scripts/install.sh handing the herd to the binary it just
// fetched, while the registry still names the PREVIOUS install. Deferring
// there would give the daemon back to the build being replaced.
func TestEnsureMayDefer(t *testing.T) {
	current, notCurrent := func() bool { return true }, func() bool { return false }
	tests := []struct {
		name        string
		replaceOnly bool
		current     func() bool
		want        bool
	}{
		{"start or replace: ask", false, notCurrent, true},
		{"daemon already current: nothing would happen", false, current, false},
		{"installer handover: never", true, notCurrent, false},
		{"installer handover, current", true, current, false},
	}
	for _, tt := range tests {
		if got := ensureMayDefer(tt.replaceOnly, tt.current); got != tt.want {
			t.Errorf("%s: ensureMayDefer = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// A restart "whatever binary it came from" is about the daemon being stopped;
// the one started must still be the install herdr runs.
func TestRestartFromASupersededInstallDefers(t *testing.T) {
	paths := supersedePaths(t)
	h := stubSupersession(t, newInstall, nil)

	var out bytes.Buffer
	if err := restartDaemon(paths, &out); err != nil {
		t.Fatalf("restartDaemon: %v", err)
	}
	if h.bin != newInstall || !slices.Equal(h.args, []string{"daemon", "--restart"}) {
		t.Fatalf("handed to %q %v, want %q [daemon --restart]", h.bin, h.args, newInstall)
	}
	if out.Len() != 0 {
		t.Errorf("the superseded binary reported a restart of its own:\n%s", out.String())
	}
}

func TestDeferToRegisteredInstall(t *testing.T) {
	const self = "/plugins/github-installations/herd-auto-prompter-abc/1-0/checkout/bin/hap"

	t.Run("the installed binary carries on itself", func(t *testing.T) {
		supersedePaths(t)
		h := stubSupersession(t, "", nil)
		if deferred, err := deferToRegisteredInstall(self, "daemon", "--ensure"); deferred || err != nil {
			t.Fatalf("deferred=%v err=%v, want the caller to carry on", deferred, err)
		}
		if h.bin != "" {
			t.Errorf("ran %q with nothing registered in its place", h.bin)
		}
	})

	// A registry that changes its answer between the two processes must not
	// bounce the request back: the handed-off run never defers again.
	t.Run("a handed-off run never defers again", func(t *testing.T) {
		supersedePaths(t)
		t.Setenv(delegatedEnv, "1")
		h := stubSupersession(t, newInstall, nil)
		if deferred, _ := deferToRegisteredInstall(self, "daemon", "--ensure"); deferred {
			t.Fatal("a delegated run deferred again")
		}
		if h.asked != 0 {
			t.Errorf("asked the registry %d time(s) on a delegated run", h.asked)
		}
	})

	// The hand-off IS this command's outcome: its failure must surface, and
	// must not fall through to starting the superseded build after all.
	t.Run("a failed hand-off is reported, not papered over", func(t *testing.T) {
		supersedePaths(t)
		boom := errors.New("exit status 1")
		stubSupersession(t, newInstall, boom)
		deferred, err := deferToRegisteredInstall(self, "daemon", "--ensure")
		if !deferred || !errors.Is(err, boom) || !strings.Contains(err.Error(), newInstall) {
			t.Fatalf("deferred=%v err=%v, want the hand-off's error naming %s", deferred, err, newInstall)
		}
	})
}
