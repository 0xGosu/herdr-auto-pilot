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
