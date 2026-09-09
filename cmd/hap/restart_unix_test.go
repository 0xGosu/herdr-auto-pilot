//go:build !windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/cli"
	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/crashguard"
	"github.com/0xGosu/herdr-auto-pilot/internal/daemonhealth"
)

// holdDaemonLock simulates a running daemon: content in the lock file plus a
// flock on its own fd, which is what daemonlock.Info reads "running" from —
// writing the file alone leaves the lock free and the daemon reads as gone.
func holdDaemonLock(t *testing.T, paths config.Paths, content string) (release func()) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(paths.StateDir, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		t.Fatalf("flock: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			f.Close()
		})
	}
	t.Cleanup(release)
	return release
}

// TestRestartReportsWhatItActuallyDid pins the three outcomes --restart can
// have, because each is a different fact about the herd and none is visible
// afterwards: it replaced a daemon, there was none to replace, or it started
// one that never reported healthy.
//
// The last is the one that matters. spawnDaemon returns when the FORK
// succeeds, and every [database] failure exits AFTER the new daemon takes the
// lock — so an unconfirmed start must never print as success by the very
// command that just killed a working daemon to get there.
func TestRestartReportsWhatItActuallyDid(t *testing.T) {
	t.Run("replaces a running daemon", func(t *testing.T) {
		paths := config.Paths{ConfigDir: t.TempDir(), StateDir: t.TempDir()}
		release := holdDaemonLock(t, paths, "4242\nv0.8.3\n/usr/local/bin/hap\n")
		var out strings.Builder
		started := 0
		err := restartWith(paths, &out,
			func(int) error { release(); return nil }, // a stopped daemon frees the lock
			func() error { started++; return nil },
			func(int, time.Time) (int, bool) { return 5150, true })
		if err != nil {
			t.Fatalf("restartWith: %v", err)
		}
		if started != 1 {
			t.Fatalf("start called %d times, want 1", started)
		}
		for _, want := range []string{"stopped the daemon (pid 4242, v0.8.3)", "pid 5150", "reporting healthy"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("report missing %q, got:\n%s", want, out.String())
			}
		}
	})

	t.Run("starts one when none was running", func(t *testing.T) {
		paths := config.Paths{ConfigDir: t.TempDir(), StateDir: t.TempDir()}
		var out strings.Builder
		err := restartWith(paths, &out,
			func(int) error { t.Fatal("nothing was running; stop must not be called"); return nil },
			func() error { return nil },
			func(int, time.Time) (int, bool) { return 5150, true })
		if err != nil {
			t.Fatalf("restartWith: %v", err)
		}
		if !strings.Contains(out.String(), "no daemon was running") {
			t.Errorf("report must not claim it stopped one, got:\n%s", out.String())
		}
		if strings.Contains(out.String(), "stopped the daemon") {
			t.Errorf("nothing was stopped, got:\n%s", out.String())
		}
	})

	t.Run("an unconfirmed start is never reported as success", func(t *testing.T) {
		paths := config.Paths{ConfigDir: t.TempDir(), StateDir: t.TempDir()}
		release := holdDaemonLock(t, paths, "4242\nv0.8.3\n/usr/local/bin/hap\n")
		var out strings.Builder
		err := restartWith(paths, &out,
			func(int) error { release(); return nil },
			func() error { return nil },
			func(int, time.Time) (int, bool) { return 0, false })
		// Non-zero exit, or `hap daemon --restart && <next step>` proceeds on
		// an unknown — with the herd possibly left unmonitored, since a daemon
		// was stopped to get here. The sentinel is what suppresses a redundant
		// "error:" line over the diagnostic below.
		if !errors.Is(err, cli.ErrUnhealthy) {
			t.Fatalf("restartWith = %v, want cli.ErrUnhealthy so the process exits non-zero", err)
		}
		if strings.Contains(out.String(), "reporting healthy") {
			t.Fatalf("an unconfirmed start must not read as healthy, got:\n%s", out.String())
		}
		for _, want := range []string{"has not reported healthy", "hap status", "[database]"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("report missing %q, got:\n%s", want, out.String())
			}
		}
	})
}

// TestRestartDoesNotFeedTheCrashLoopBreaker guards the flag's own use case:
// `config set` + --restart, repeated while getting a [database] setting right,
// is a normal minute's work — and three boots inside crashguard.Window latch
// the embedder OFF (two more stop the daemon entirely, for herdr's hook too).
// The boot history is therefore cleared by an operator-typed restart, while
// every LATCH is left exactly as it was for spawnBlocked to enforce.
func TestRestartDoesNotFeedTheCrashLoopBreaker(t *testing.T) {
	paths := config.Paths{ConfigDir: t.TempDir(), StateDir: t.TempDir()}
	now := time.Now()
	if err := crashguard.Write(paths.StateDir, crashguard.State{
		Starts:       []time.Time{now, now},
		EmbeddingOff: true,
		Reason:       "latched earlier",
		ConfigDigest: "digest",
	}); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := restartWith(paths, &out,
		func(int) error { return nil },
		func() error { return nil },
		func(int, time.Time) (int, bool) { return 5150, true }); err != nil {
		t.Fatalf("restartWith: %v", err)
	}
	g, ok := crashguard.Read(paths.StateDir)
	if !ok {
		t.Fatal("crashguard state disappeared")
	}
	if len(g.Starts) != 0 {
		t.Errorf("boot history = %d entries, want it cleared so a restart is not read as a crash", len(g.Starts))
	}
	// Clearing the EVIDENCE is safe; clearing a LATCH would let a restart
	// defeat the breaker the herdr hook depends on.
	if !g.EmbeddingOff || g.Reason != "latched earlier" || g.ConfigDigest != "digest" {
		t.Errorf("a restart must not clear the breaker's latches, got %+v", g)
	}
}

// TestAwaitDaemonHealthOnlyBelievesTheSuccessor is the guard the whole
// confirmation rests on. Reading somebody else's heartbeat as the successor's
// is not a cosmetic slip: it restores the exact false success --restart exists
// to remove, reporting "healthy" for a daemon that died on the [database]
// error — from the command that just stopped a working one.
//
// Each subtest removes one of the three conditions' evidence and expects a
// refusal; the last one is the control, without which they could all pass
// vacuously.
func TestAwaitDaemonHealthOnlyBelievesTheSuccessor(t *testing.T) {
	// beat writes a health record and returns the paths it lives in. Callers
	// pass a timestamp derived from `since` rather than a second time.Now():
	// the two calls are microseconds apart, After is strict, and macOS's wall
	// clock is coarse enough that they can land on the SAME instant — which
	// refused the successor on macOS while passing on Linux, and would also
	// have made the two refusal cases below pass for the wrong reason.
	beat := func(t *testing.T, pid int, at time.Time) config.Paths {
		t.Helper()
		paths := config.Paths{ConfigDir: t.TempDir(), StateDir: t.TempDir()}
		if err := daemonhealth.Write(paths.StateDir, daemonhealth.Health{
			PID: pid, Version: "v0.8.3", HeartbeatAt: at,
		}); err != nil {
			t.Fatal(err)
		}
		return paths
	}

	t.Run("a record older than the start is a leftover", func(t *testing.T) {
		// A hard-killed predecessor: different pid, written seconds BEFORE the
		// restart and well inside daemonhealth.StaleAfter, so an age test
		// would wave it through. stoppedPID is 0 here — nothing was running —
		// which is what makes the pid check alone useless.
		since := time.Now()
		paths := beat(t, 1234, since.Add(-2*time.Second))
		holdDaemonLock(t, paths, "1234\nv0.8.3\n/usr/local/bin/hap\n")
		if pid, ok := awaitDaemonHealth(paths, 0, since, 300*time.Millisecond); ok {
			t.Fatalf("a leftover heartbeat was read as the successor (pid %d)", pid)
		}
	})

	t.Run("the daemon we stopped never counts", func(t *testing.T) {
		since := time.Now()
		paths := beat(t, 4242, since.Add(time.Second))
		holdDaemonLock(t, paths, "4242\nv0.8.3\n/usr/local/bin/hap\n")
		if _, ok := awaitDaemonHealth(paths, 4242, since, 300*time.Millisecond); ok {
			t.Fatal("the stopped daemon's own final beat was read as its successor")
		}
	})

	t.Run("a beat from a process that no longer holds the lock", func(t *testing.T) {
		// The successor published one heartbeat and then died — fresh, after
		// the start, right pid. Only the lock says it is gone, because the
		// kernel drops the flock with the process while the file it wrote
		// stays on disk.
		since := time.Now()
		paths := beat(t, 5150, since.Add(time.Second))
		if pid, ok := awaitDaemonHealth(paths, 0, since, 300*time.Millisecond); ok {
			t.Fatalf("a dead daemon's last beat was read as healthy (pid %d)", pid)
		}
	})

	t.Run("the successor itself is accepted", func(t *testing.T) {
		since := time.Now()
		paths := beat(t, 5150, since.Add(time.Second))
		holdDaemonLock(t, paths, "5150\nv0.8.3\n/usr/local/bin/hap\n")
		pid, ok := awaitDaemonHealth(paths, 0, since, 2*time.Second)
		if !ok || pid != 5150 {
			t.Fatalf("awaitDaemonHealth = (%d, %v), want the successor's pid — without this the refusals above prove nothing", pid, ok)
		}
	})
}

// TestSyncRecoveryOrdersARESTARTNotAnEnsure pins the one choice this seam
// cannot get wrong. The upgrade handoff spawns `--ensure` because its successor
// is a DIFFERENT binary; a sync recovery restarts as the SAME one, and
// EnsureFresh returns doing nothing when the running holder already matches the
// version and path it would start (daemonlock's own
// TestRestartReplacesAHolderEnsureFreshWouldKeep is that behaviour pinned from
// the other side). So an `--ensure` here would start nothing at all, and the
// isolation it was meant to repair would simply continue — with the operator
// told, in the log, that a replacement had been started.
func TestSyncRecoveryOrdersARestartNotAnEnsure(t *testing.T) {
	paths := config.Paths{StateDir: t.TempDir(), ConfigDir: t.TempDir()}
	var got []string
	hook := restartSelfHook(paths, true, func(_ string, args ...string) error {
		got = args
		return nil
	})
	if hook == nil {
		t.Fatal("no recovery seam was built under the turso engine")
	}
	if err := hook("fleet sync wedged: SecPolicyCreateSSL error: 0"); err != nil {
		t.Fatalf("hook: %v", err)
	}
	if len(got) != 2 || got[0] != "daemon" || got[1] != "--restart" {
		t.Fatalf("spawned %q, want [daemon --restart]", got)
	}
}

// TestSyncRecoverySeamIsAbsentWithoutASharedDatabase: under the local engine
// there is no fleet to be isolated from, so the daemon is handed no way to
// restart itself at all.
func TestSyncRecoverySeamIsAbsentWithoutASharedDatabase(t *testing.T) {
	paths := config.Paths{StateDir: t.TempDir(), ConfigDir: t.TempDir()}
	if restartSelfHook(paths, false, func(string, ...string) error {
		t.Fatal("spawned a daemon under the local engine")
		return nil
	}) != nil {
		t.Fatal("a recovery seam was built under the local engine")
	}
}

// TestSyncRecoveryRefusesWhileTheCrashLoopBreakerHasGivenUp: a latched breaker
// refuses the start AFTER the fork has already returned nil, so the daemon
// would read a spawn that succeeded. Refusing here keeps this daemon up —
// isolated is bad, unmonitored is worse.
func TestSyncRecoveryRefusesWhileTheCrashLoopBreakerHasGivenUp(t *testing.T) {
	paths := config.Paths{StateDir: t.TempDir(), ConfigDir: t.TempDir()}
	if err := crashguard.Write(paths.StateDir, crashguard.State{
		GaveUp: true, Reason: "native abort in the embedder",
	}); err != nil {
		t.Fatal(err)
	}
	hook := restartSelfHook(paths, true, func(string, ...string) error {
		t.Fatal("spawned a daemon the crash-loop breaker would suppress")
		return nil
	})
	err := hook("fleet sync wedged: too many open files")
	if err == nil || !strings.Contains(err.Error(), "crash-loop breaker") {
		t.Fatalf("error = %v, want a crash-loop breaker refusal", err)
	}
}
