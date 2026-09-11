package profiling

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/pprof"
	"testing"
	"time"
)

// CPU profiling is process-global, so none of these tests run in parallel.

func burn(d time.Duration) {
	x := 0
	for end := time.Now().Add(d); time.Now().Before(end); {
		x++
	}
	_ = x
}

// gzipped reports whether path holds a pprof file: gzip-compressed protobuf.
func gzipped(t *testing.T, path string) bool {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return len(b) > 2 && bytes.Equal(b[:2], []byte{0x1f, 0x8b})
}

// cpuProfilingIdle reports whether nothing holds the process's CPU profiler.
func cpuProfilingIdle() bool {
	if err := pprof.StartCPUProfile(io.Discard); err != nil {
		return false
	}
	pprof.StopCPUProfile()
	return true
}

func TestFromEnvIsOffUnlessTheDirIsSet(t *testing.T) {
	t.Setenv(EnvDir, "")
	stop := FromEnv("daemon")
	defer stop()
	if !cpuProfilingIdle() {
		t.Error("profiling started with HAP_PROFILE_DIR unset")
	}
}

// TestStartWritesRollingWindows: a long-lived process must have a COMPLETE
// window on disk while it is still running — that is the whole point of
// rolling — and stop must finalize the one in progress and leave no partial
// files behind.
func TestStartWritesRollingWindows(t *testing.T) {
	dir := t.TempDir()
	stop, err := Start(dir, "daemon", 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(dir, fmt.Sprintf("daemon-%d", os.Getpid()))
	deadline := time.Now().Add(5 * time.Second)
	for {
		burn(20 * time.Millisecond)
		if _, err := os.Stat(base + ".cpu.pprof"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			stop()
			t.Fatal("no completed CPU window appeared while the process ran")
		}
	}
	stop()
	stop() // idempotent

	for _, p := range []string{base + ".cpu.pprof", base + ".heap.pprof"} {
		if !gzipped(t, p) {
			t.Errorf("%s is not a pprof file", p)
		}
	}
	partials, _ := filepath.Glob(filepath.Join(dir, "*.partial"))
	if len(partials) > 0 {
		t.Errorf("stop left partial files behind: %v", partials)
	}
	if !cpuProfilingIdle() {
		t.Error("stop did not release the CPU profiler")
	}
}

// TestFromEnvDegradesWhenTheProfilerIsTaken: a second profiler in one process
// (a test harness, a library) must cost the operator the profile, never the
// command.
func TestFromEnvDegradesWhenTheProfilerIsTaken(t *testing.T) {
	if err := pprof.StartCPUProfile(io.Discard); err != nil {
		t.Fatal(err)
	}
	defer pprof.StopCPUProfile()
	t.Setenv(EnvDir, t.TempDir())
	stop := FromEnv("daemon")
	stop()
}

func TestStartKeepsFilesInsideTheDir(t *testing.T) {
	dir := t.TempDir()
	stop, err := Start(dir, "../../escape", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	stop()
	matches, _ := filepath.Glob(filepath.Join(dir, "*.pprof"))
	if len(matches) != 2 {
		t.Errorf("want the cpu and heap profile inside %s, got %v", dir, matches)
	}
}

func TestStartRefusesANonPositiveWindow(t *testing.T) {
	if _, err := Start(t.TempDir(), "daemon", 0); err == nil {
		t.Error("a zero window was accepted")
	}
}
