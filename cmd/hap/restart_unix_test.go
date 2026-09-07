//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

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
		if err != nil {
			t.Fatalf("restartWith: %v", err)
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

// TestAwaitDaemonHealthIgnoresALeftoverRecord is the guard the whole
// confirmation rests on. Reading somebody else's heartbeat as the successor's
// is not a cosmetic slip: it restores the exact false success --restart exists
// to remove, reporting "healthy" for a daemon that died on the [database]
// error, and it reports it from the command that just stopped a working one.
//
// The pid check alone does not close it, which is why the freshness test is
// against the restart's own start instant rather than an age: a daemon killed
// hard seconds ago — or, when nothing was running, any daemon that ever ran
// here — leaves a record with a different pid that still looks recent.
func TestAwaitDaemonHealthIgnoresALeftoverRecord(t *testing.T) {
	paths := config.Paths{ConfigDir: t.TempDir(), StateDir: t.TempDir()}
	since := time.Now()

	// A record from a hard-killed predecessor: different pid, written moments
	// BEFORE this restart, and well inside daemonhealth.StaleAfter.
	if err := daemonhealth.Write(paths.StateDir, daemonhealth.Health{
		PID: 1234, Version: "v0.8.3", HeartbeatAt: since.Add(-2 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if pid, ok := awaitDaemonHealth(paths, 0, since, 300*time.Millisecond); ok {
		t.Fatalf("a leftover heartbeat was read as the successor (pid %d)", pid)
	}

	// The successor's own beat, published after the start, is accepted.
	if err := daemonhealth.Write(paths.StateDir, daemonhealth.Health{
		PID: 5150, Version: "v0.8.3", HeartbeatAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	pid, ok := awaitDaemonHealth(paths, 0, since, 2*time.Second)
	if !ok || pid != 5150 {
		t.Fatalf("awaitDaemonHealth = (%d, %v), want the successor's pid — the control that keeps the case above from passing vacuously", pid, ok)
	}

	// The pid we just stopped never counts, however fresh its last beat.
	if err := daemonhealth.Write(paths.StateDir, daemonhealth.Health{
		PID: 4242, Version: "v0.8.3", HeartbeatAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := awaitDaemonHealth(paths, 4242, since, 300*time.Millisecond); ok {
		t.Fatal("the stopped daemon's own final beat was read as its successor")
	}
}
