//go:build !windows

package daemonlock_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/buildinfo"
	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/daemonlock"
)

func testPaths(t *testing.T) config.Paths {
	t.Helper()
	return config.Paths{StateDir: t.TempDir()}
}

func lockFile(paths config.Paths) string {
	return filepath.Join(paths.StateDir, "daemon.lock")
}

// holdLock simulates a running daemon: it writes content to the lock file
// and flocks it on its own fd (flock conflicts apply between separate open
// file descriptions even within one process).
func holdLock(t *testing.T, paths config.Paths, content string) (release func()) {
	t.Helper()
	f, err := os.OpenFile(lockFile(paths), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		t.Fatalf("flock: %v", err)
	}
	if err := f.Truncate(0); err != nil {
		t.Fatal(err)
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

func TestAcquireWritesPidAndVersion(t *testing.T) {
	paths := testPaths(t)
	lk, err := daemonlock.Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()
	data, err := os.ReadFile(lockFile(paths))
	if err != nil {
		t.Fatal(err)
	}
	self, _ := os.Executable()
	want := fmt.Sprintf("%d\n%s\n%s\n", os.Getpid(), buildinfo.Version, self)
	if string(data) != want {
		t.Errorf("lock file = %q, want %q", data, want)
	}
}

func TestAcquireTruncatesStaleContent(t *testing.T) {
	paths := testPaths(t)
	if err := os.WriteFile(lockFile(paths), []byte(strings.Repeat("x", 200)), 0o600); err != nil {
		t.Fatal(err)
	}
	lk, err := daemonlock.Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()
	data, err := os.ReadFile(lockFile(paths))
	if err != nil {
		t.Fatal(err)
	}
	self, _ := os.Executable()
	want := fmt.Sprintf("%d\n%s\n%s\n", os.Getpid(), buildinfo.Version, self)
	if string(data) != want {
		t.Errorf("stale bytes must not survive acquire: got %q, want %q", data, want)
	}
}

func TestAcquireFailsWhileHeld(t *testing.T) {
	paths := testPaths(t)
	holdLock(t, paths, "1\nv0\n")
	if lk, err := daemonlock.Acquire(paths); err == nil {
		lk.Release()
		t.Fatal("second Acquire must fail while the lock is held")
	}
}

func TestInfoWhileHeld(t *testing.T) {
	paths := testPaths(t)
	lk, err := daemonlock.Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()
	running, pid, version := daemonlock.Info(paths)
	if !running || pid != os.Getpid() || version != buildinfo.Version {
		t.Errorf("Info = (%v, %d, %q), want (true, %d, %q)",
			running, pid, version, os.Getpid(), buildinfo.Version)
	}
}

func TestInfoNotRunning(t *testing.T) {
	paths := testPaths(t)
	if running, pid, version := daemonlock.Info(paths); running || pid != 0 || version != "" {
		t.Errorf("Info on idle dir = (%v, %d, %q), want (false, 0, \"\")", running, pid, version)
	}
}

func TestInfoLegacyPidOnlyFormat(t *testing.T) {
	paths := testPaths(t)
	holdLock(t, paths, "1234\n")
	running, pid, version := daemonlock.Info(paths)
	if !running || pid != 1234 || version != "" {
		t.Errorf("Info = (%v, %d, %q), want (true, 1234, \"\")", running, pid, version)
	}
}

// The lock gained a third line (the holder's binary path). A daemon written by
// an older hap has only two, and must still be identified rather than read as
// a corrupt lock nobody can replace.
func TestInfoLegacyTwoLineFormat(t *testing.T) {
	paths := testPaths(t)
	holdLock(t, paths, "1234\nv0.1.0\n")
	running, pid, version := daemonlock.Info(paths)
	if !running || pid != 1234 || version != "v0.1.0" {
		t.Errorf("Info = (%v, %d, %q), want (true, 1234, \"v0.1.0\")", running, pid, version)
	}
}

func TestAcquireRecordsBinaryPath(t *testing.T) {
	paths := testPaths(t)
	lk, err := daemonlock.Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()

	data, err := os.ReadFile(lockFile(paths))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("lock file = %q, want three lines (pid, version, exe path)", data)
	}
	self, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable unavailable: %v", err)
	}
	if lines[2] != self {
		t.Errorf("recorded exe path = %q, want the running binary %q", lines[2], self)
	}
}

func TestInfoUnparsableContent(t *testing.T) {
	paths := testPaths(t)
	holdLock(t, paths, "not-a-pid\n")
	running, pid, _ := daemonlock.Info(paths)
	if !running || pid != 0 {
		t.Errorf("Info = (%v, %d), want (true, 0)", running, pid)
	}
}

func TestWaitReleased(t *testing.T) {
	paths := testPaths(t)
	release := holdLock(t, paths, "1\nv0\n")
	go func() {
		time.Sleep(200 * time.Millisecond)
		release()
	}()
	if !daemonlock.WaitReleased(paths, 3*time.Second) {
		t.Error("WaitReleased must observe the release within the timeout")
	}
}

func TestWaitReleasedTimesOut(t *testing.T) {
	paths := testPaths(t)
	holdLock(t, paths, "1\nv0\n")
	if daemonlock.WaitReleased(paths, 250*time.Millisecond) {
		t.Error("WaitReleased must report false while the lock stays held")
	}
}

func TestStopTerminatesProcessAndToleratesGonePid(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	if err := daemonlock.Stop(cmd.Process.Pid); err != nil {
		t.Fatalf("Stop live process: %v", err)
	}
	if err := cmd.Wait(); err == nil || !strings.Contains(err.Error(), "terminated") {
		t.Errorf("process must die from SIGTERM, Wait() = %v", err)
	}
	// Reaped now, so the pid is gone: ESRCH must not surface as an error.
	if err := daemonlock.Stop(cmd.Process.Pid); err != nil {
		t.Errorf("Stop on a gone pid = %v, want nil", err)
	}
}

// writeExe creates a file with the execute bit, standing in for an installed
// hap binary at a particular path.
func writeExe(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestEnsureFreshPathStaleness covers the upgrade shape the version check
// alone cannot see: a holder running the SAME version from a binary that is
// no longer the one we would start. Its children (the MCP server, the embed
// worker) are spawned by path, so it must be replaced even though nothing
// about the version says so.
func TestEnsureFreshPathStaleness(t *testing.T) {
	dir := t.TempDir()
	oldExe := writeExe(t, filepath.Join(dir, "old", "bin", "hap"))
	newExe := writeExe(t, filepath.Join(dir, "new", "bin", "hap"))
	// A symlink to the current binary is how an operator's /usr/local/bin/hap
	// reaches it; that must NOT read as a different install.
	linked := filepath.Join(dir, "usr-local-hap")
	if err := os.Symlink(newExe, linked); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name        string
		lockContent string
		exePath     string
		wantStop    bool
	}{
		{
			name:        "same version, replaced binary is stale",
			lockContent: "4242\nv0.1.0\n" + oldExe + "\n",
			exePath:     newExe,
			wantStop:    true,
		},
		{
			name:        "same version and same binary is current",
			lockContent: "4242\nv0.1.0\n" + newExe + "\n",
			exePath:     newExe,
		},
		{
			name:        "path reached through a symlink is the same binary",
			lockContent: "4242\nv0.1.0\n" + newExe + "\n",
			exePath:     linked,
		},
		{
			name: "holder recorded no path: version alone decides",
			// A daemon from before the path was recorded. Killing it on an
			// unknowable path would make every --ensure a restart.
			lockContent: "4242\nv0.1.0\n",
			exePath:     newExe,
		},
		{
			name:        "our own path is unknown: version alone decides",
			lockContent: "4242\nv0.1.0\n" + oldExe + "\n",
			exePath:     "",
		},
		{
			name: "holder path no longer exists but matches ours",
			// Both sides name the same removed file. It cannot be resolved, so
			// the comparison is skipped rather than treated as a mismatch.
			lockContent: "4242\nv0.1.0\n" + filepath.Join(dir, "gone", "hap") + "\n",
			exePath:     filepath.Join(dir, "gone", "hap"),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			paths := testPaths(t)
			release := holdLock(t, paths, tc.lockContent)
			var stopped, started int
			stop := func(int) error {
				stopped++
				release()
				return nil
			}
			start := func() error { started++; return nil }

			if err := daemonlock.EnsureFresh(paths, "v0.1.0", tc.exePath, 300*time.Millisecond, stop, start); err != nil {
				t.Fatalf("EnsureFresh: %v", err)
			}
			if (stopped > 0) != tc.wantStop {
				t.Errorf("stop called = %v, want %v", stopped > 0, tc.wantStop)
			}
			if (started > 0) != tc.wantStop {
				t.Errorf("start called = %v, want %v", started > 0, tc.wantStop)
			}
		})
	}
}

func TestEnsureFresh(t *testing.T) {
	const held = "4242\nv0.1.0\n"
	tests := []struct {
		name        string
		lockContent string // "" = no daemon running
		version     string // current binary version passed to EnsureFresh
		exePath     string // current binary path passed to EnsureFresh ("" = unknown)
		stopFrees   bool   // stop releases the lock (daemon exits)
		stopErr     bool   // stop itself fails
		wantErr     bool
		wantStop    bool
		wantStart   bool
		wantStopPid int
	}{
		{name: "not running starts", version: "v0.1.0", wantStart: true},
		{name: "same version no-op", lockContent: held, version: "v0.1.0"},
		{name: "mismatch restarts", lockContent: held, version: "v0.2.0",
			stopFrees: true, wantStop: true, wantStart: true, wantStopPid: 4242},
		{name: "legacy pid-only restarts", lockContent: "4242\n", version: "v0.2.0",
			stopFrees: true, wantStop: true, wantStart: true, wantStopPid: 4242},
		{name: "holder never exits errors without start", lockContent: held, version: "v0.2.0",
			stopFrees: false, wantStop: true, wantErr: true},
		{name: "unreadable pid errors without kill", lockContent: "junk\nv0.1.0\n", version: "v0.2.0",
			wantErr: true},
		{name: "stop failure surfaces without start", lockContent: held, version: "v0.2.0",
			stopErr: true, wantStop: true, wantErr: true, wantStopPid: 4242},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			paths := testPaths(t)
			var release func()
			if tc.lockContent != "" {
				release = holdLock(t, paths, tc.lockContent)
			}
			var stopped, started []int
			stop := func(pid int) error {
				stopped = append(stopped, pid)
				if tc.stopErr {
					return fmt.Errorf("kill refused")
				}
				if tc.stopFrees {
					release()
				}
				return nil
			}
			start := func() error {
				started = append(started, 1)
				return nil
			}
			err := daemonlock.EnsureFresh(paths, tc.version, tc.exePath, 300*time.Millisecond, stop, start)
			if (err != nil) != tc.wantErr {
				t.Fatalf("EnsureFresh error = %v, wantErr %v", err, tc.wantErr)
			}
			if got := len(stopped) > 0; got != tc.wantStop {
				t.Errorf("stop called = %v, want %v", got, tc.wantStop)
			}
			if tc.wantStop && tc.wantStopPid != 0 && (len(stopped) == 0 || stopped[0] != tc.wantStopPid) {
				t.Errorf("stop pid = %v, want %d", stopped, tc.wantStopPid)
			}
			if got := len(started) > 0; got != tc.wantStart {
				t.Errorf("start called = %v, want %v", got, tc.wantStart)
			}
		})
	}
}

// TestRestartReplacesAHolderEnsureFreshWouldKeep is the whole reason --restart
// exists: EnsureFresh returns early when the running daemon is already this
// version at this path, so before it nothing in hap could pick up a [database]
// change — the section is read once, when a process opens its store — without
// an operator finding the pid and sending SIGTERM by hand. Both calls are
// driven against the SAME held lock, so the flag is the only difference.
func TestRestartReplacesAHolderEnsureFreshWouldKeep(t *testing.T) {
	paths := testPaths(t)
	release := holdLock(t, paths, fmt.Sprintf("4242\n%s\n/usr/local/bin/hap\n", buildinfo.Version))

	var stopped, started []int
	stop := func(pid int) error {
		stopped = append(stopped, pid)
		release() // a stopped daemon releases the lock; WaitReleased polls for it
		return nil
	}
	start := func() error { started = append(started, 1); return nil }

	if err := daemonlock.EnsureFresh(paths, buildinfo.Version, "/usr/local/bin/hap",
		300*time.Millisecond, stop, start); err != nil {
		t.Fatalf("EnsureFresh: %v", err)
	}
	if len(stopped) != 0 || len(started) != 0 {
		t.Fatalf("--ensure must leave a current daemon alone: stopped=%v started=%v", stopped, started)
	}

	outcome, err := daemonlock.Restart(paths, 300*time.Millisecond, stop, start)
	if err != nil {
		t.Fatalf("Restart: %v", err)
	}
	// The outcome is what the CLI reports from, so it must name the daemon it
	// actually stopped rather than leave the caller to re-read Info() and race
	// the successor it just started.
	if !outcome.Replaced || outcome.StoppedPID != 4242 || outcome.StoppedVersion != buildinfo.Version {
		t.Errorf("outcome = %+v, want the stopped daemon described", outcome)
	}
	if len(stopped) != 1 || stopped[0] != 4242 {
		t.Errorf("--restart must stop the running daemon, stopped=%v", stopped)
	}
	if len(started) != 1 {
		t.Errorf("--restart must start a replacement, started=%v", started)
	}
}

// TestRestart covers the rest of the contract by driving the same table shape
// as TestEnsureFresh. The cases that matter beyond "it stops a current holder"
// are the ones a parallel implementation would drop: with nothing running it
// STARTS one (an operator recovering a dead daemon must not need a different
// verb), and a holder that never exits is an error with no start — starting a
// second daemon over one still holding the lock is how you get two monitors,
// or a replacement that dies on Acquire.
func TestRestart(t *testing.T) {
	const held = "4242\nv0.1.0\n/usr/local/bin/hap\n"
	tests := []struct {
		name        string
		lockContent string // "" = no daemon running
		stopFrees   bool
		stopErr     bool
		wantErr     bool
		wantStop    bool
		wantStart   bool
	}{
		{name: "nothing running starts one", wantStart: true},
		// This case does NOT pin `force`: Restart passes version="", so
		// `current("v0.1.0", …, "", "")` is false and the holder would be
		// replaced either way. Only a holder `current` says yes to reaches the
		// guard — the legacy pid-only lock below, where every string is empty
		// (verified by mutation: dropping `!force` fails that case alone).
		{name: "a current-looking holder is still replaced", lockContent: held,
			stopFrees: true, wantStop: true, wantStart: true},
		{name: "legacy pid-only holder is replaced", lockContent: "4242\n",
			stopFrees: true, wantStop: true, wantStart: true},
		{name: "holder never exits errors without start", lockContent: held,
			wantStop: true, wantErr: true},
		{name: "unreadable pid errors without kill", lockContent: "junk\nv0.1.0\n", wantErr: true},
		{name: "stop failure surfaces without start", lockContent: held,
			stopErr: true, wantStop: true, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			paths := testPaths(t)
			var release func()
			if tc.lockContent != "" {
				release = holdLock(t, paths, tc.lockContent)
			}
			var stopped, started []int
			stop := func(pid int) error {
				stopped = append(stopped, pid)
				if tc.stopErr {
					return fmt.Errorf("kill refused")
				}
				if tc.stopFrees {
					release()
				}
				return nil
			}
			start := func() error { started = append(started, 1); return nil }

			outcome, err := daemonlock.Restart(paths, 300*time.Millisecond, stop, start)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Restart error = %v, wantErr %v", err, tc.wantErr)
			}
			if got := len(stopped) > 0; got != tc.wantStop {
				t.Errorf("stop called = %v, want %v", got, tc.wantStop)
			}
			if got := len(started) > 0; got != tc.wantStart {
				t.Errorf("start called = %v, want %v", got, tc.wantStart)
			}
			// The outcome is what the CLI reports from: it must describe a
			// replacement only when one actually happened, and never after an
			// error (nothing was started, so "stopped … and started" would be
			// a claim about a herd that has no daemon at all).
			if outcome.Replaced != (tc.wantStop && !tc.wantErr) {
				t.Errorf("outcome = %+v, want Replaced=%v", outcome, tc.wantStop && !tc.wantErr)
			}
		})
	}
}

// TestRestartForcesThroughTheReProbeToo pins the SECOND copy of the force
// guard. A holder that rewrites its lock file is read once with an unparseable
// pid, so replaceHolder sleeps 100ms and reads again — and on that path the
// re-probe re-asks `current`, which a legacy pid-only lock answers yes to.
// Without `!force` there, a restart returns nil having done nothing while the
// CLI still reports a fresh daemon and the [database] change never lands.
func TestRestartForcesThroughTheReProbeToo(t *testing.T) {
	paths := testPaths(t)
	release := holdLock(t, paths, "junk\n")
	// Rewrite the lock content during the re-probe window, the way a daemon
	// that truncates then rewrites its own lock file does.
	go func() {
		time.Sleep(30 * time.Millisecond)
		f, err := os.OpenFile(lockFile(paths), os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		defer f.Close()
		f.Truncate(0)
		f.WriteString("4242\n")
	}()

	var stopped, started []int
	outcome, err := daemonlock.Restart(paths, 300*time.Millisecond,
		func(pid int) error { stopped = append(stopped, pid); release(); return nil },
		func() error { started = append(started, 1); return nil })
	if err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if len(stopped) != 1 || stopped[0] != 4242 {
		t.Errorf("stopped = %v, want the pid the re-probe read", stopped)
	}
	if len(started) != 1 || !outcome.Replaced {
		t.Errorf("started = %v, outcome = %+v, want a replacement", started, outcome)
	}
}
