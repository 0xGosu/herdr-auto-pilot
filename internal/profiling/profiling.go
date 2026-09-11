// Package profiling is hap's opt-in CPU and heap profiling hook. Nothing here
// runs unless the operator sets HAP_PROFILE_DIR, and it writes only to that
// directory — no listener, no egress (net/http/pprof would be an HTTP server,
// which internal/privacy bans for good reason).
//
// Profiles are ROLLING windows rather than one whole-process capture, because
// the process worth profiling is usually a daemon that has been idle for
// hours: every HAP_PROFILE_SECONDS the current CPU window is closed and
// renamed into place, a heap snapshot is written beside it, and a new window
// starts. The files on disk are therefore always the LATEST complete window,
// and a process that exits finalizes the window it was in.
//
// Each heap snapshot gets a <name>-<pid>.mem.txt beside it: the Go runtime's
// memory classes (runtime/metrics) and the process's own RSS lines. A heap
// profile only sees live Go objects, and for hap those are a few MB of a
// resident set several times larger — the rest is Go memory freed but not yet
// returned to the OS, goroutine stacks, and the NATIVE side (llama.cpp, FAISS,
// the Turso engine, malloc arenas). The file is what tells those apart.
//
//	HAP_PROFILE_DIR=/tmp/hap-prof hap daemon --restart
//	go tool pprof -top /tmp/hap-prof/daemon-<pid>.cpu.pprof
package profiling

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// EnvDir switches profiling on and names the directory profiles go to.
	EnvDir = "HAP_PROFILE_DIR"
	// EnvWindow is the rolling window length in whole seconds.
	EnvWindow = "HAP_PROFILE_SECONDS"
	// DefaultWindow is the window when EnvWindow is unset.
	DefaultWindow = 60 * time.Second
)

// FromEnv starts profiling for the process named name when HAP_PROFILE_DIR is
// set, and returns the function that finalizes it; the function is a no-op
// otherwise. A failure to start is reported on stderr and never fails the
// command — profiling is a diagnostic, not a feature anything depends on.
func FromEnv(name string) (stop func()) {
	dir := os.Getenv(EnvDir)
	if dir == "" {
		return func() {}
	}
	window := DefaultWindow
	if s := os.Getenv(EnvWindow); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			fmt.Fprintf(os.Stderr, "hap: ignoring %s=%q (want whole seconds > 0); using %s\n", EnvWindow, s, window)
		} else {
			window = time.Duration(n) * time.Second
		}
	}
	stop, err := Start(dir, name, window)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hap: profiling not started: %v\n", err)
		return func() {}
	}
	return stop
}

// Start writes rolling CPU and heap profiles for this process into dir as
// <name>-<pid>.cpu.pprof and <name>-<pid>.heap.pprof, closing a window every
// window. The returned stop finalizes the current window; calling it more
// than once is safe.
func Start(dir, name string, window time.Duration) (stop func(), err error) {
	if window <= 0 {
		return nil, fmt.Errorf("profile window must be positive, got %s", window)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("profile dir: %w", err)
	}
	p := &profiler{base: filepath.Join(dir, fmt.Sprintf("%s-%d", fileSafe(name), os.Getpid()))}
	if err := p.begin(); err != nil {
		return nil, err
	}
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(window)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				p.finish()
				if err := p.begin(); err != nil {
					slog.Warn("profiling: could not start the next CPU window; stopping", "error", err)
					return
				}
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			<-finished
			p.finish()
		})
	}, nil
}

// profiler owns one CPU window at a time. Only the rolling goroutine and, once
// that goroutine has returned, stop ever touch it, so it needs no lock.
type profiler struct {
	base string
	cpu  *os.File // the open window; nil between windows
}

func (p *profiler) begin() error {
	f, err := os.Create(p.base + ".cpu.pprof.partial")
	if err != nil {
		return fmt.Errorf("create CPU profile: %w", err)
	}
	if err := pprof.StartCPUProfile(f); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return fmt.Errorf("start CPU profile: %w", err)
	}
	p.cpu = f
	return nil
}

// finish closes the open CPU window into place and writes a heap snapshot.
// Errors are logged, never returned: a lost window must not end the process
// or the next window.
func (p *profiler) finish() {
	if p.cpu != nil {
		pprof.StopCPUProfile()
		name := p.cpu.Name()
		if err := p.cpu.Close(); err != nil {
			slog.Warn("profiling: closing the CPU window failed", "error", err)
		} else if err := os.Rename(name, p.base+".cpu.pprof"); err != nil {
			slog.Warn("profiling: publishing the CPU window failed", "error", err)
		}
		p.cpu = nil
	}
	if err := writeHeap(p.base + ".heap.pprof"); err != nil {
		slog.Warn("profiling: heap snapshot failed", "error", err)
	}
	if err := writeMem(p.base + ".mem.txt"); err != nil {
		slog.Warn("profiling: memory summary failed", "error", err)
	}
}

// memClasses are the runtime/metrics memory classes worth reading against the
// RSS. They are the large ones, not a partition (the mcache/mspan metadata and
// profiling buckets are left out), so the lines need not add up to total;
// "go resident" below is computed from total and released alone.
var memClasses = []string{
	"/memory/classes/total:bytes",
	"/memory/classes/heap/objects:bytes",
	"/memory/classes/heap/unused:bytes",
	"/memory/classes/heap/free:bytes",
	"/memory/classes/heap/released:bytes",
	"/memory/classes/heap/stacks:bytes",
	"/memory/classes/os-stacks:bytes",
	"/memory/classes/metadata/other:bytes",
	"/memory/classes/other:bytes",
	"/gc/heap/goal:bytes",
}

// writeMem writes the Go memory classes and the process's resident-set lines
// (Linux; elsewhere only the Go half), renamed into place like writeHeap.
// "Go resident" is total minus released: what the Go runtime holds that the OS
// counts; whatever RSS is beyond it and beyond file-backed pages is native.
func writeMem(path string) error {
	samples := make([]metrics.Sample, len(memClasses))
	for i, name := range memClasses {
		samples[i].Name = name
	}
	metrics.Read(samples)
	var b strings.Builder
	var total, released uint64
	for _, s := range samples {
		if s.Value.Kind() != metrics.KindUint64 {
			continue
		}
		v := s.Value.Uint64()
		switch s.Name {
		case "/memory/classes/total:bytes":
			total = v
		case "/memory/classes/heap/released:bytes":
			released = v
		}
		fmt.Fprintf(&b, "%-40s %8d kB\n", s.Name, v/1024)
	}
	fmt.Fprintf(&b, "%-40s %8d kB\n", "go resident (total - released)", (total-released)/1024)
	if status, err := os.ReadFile("/proc/self/status"); err == nil {
		for _, line := range strings.Split(string(status), "\n") {
			if strings.HasPrefix(line, "Vm") || strings.HasPrefix(line, "Rss") || strings.HasPrefix(line, "Threads") {
				b.WriteString(line + "\n")
			}
		}
	}
	tmp := path + ".partial"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// writeHeap snapshots the heap after a GC, so in-use figures describe what is
// actually retained rather than garbage not yet collected, and renames the
// file into place so a reader never sees a half-written snapshot.
func writeHeap(path string) error {
	runtime.GC()
	tmp := path + ".partial"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := pprof.Lookup("heap").WriteTo(f, 0); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// fileSafe keeps a process name usable as a file-name prefix: the verb comes
// from the command line, so a separator in it must not escape the directory.
func fileSafe(name string) string {
	s := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		}
		return '_'
	}, name)
	if s == "" {
		return "hap"
	}
	return s
}
