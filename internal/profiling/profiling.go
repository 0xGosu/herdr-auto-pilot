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
//	HAP_PROFILE_DIR=/tmp/hap-prof hap daemon --restart
//	go tool pprof -top /tmp/hap-prof/daemon-<pid>.cpu.pprof
package profiling

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
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
