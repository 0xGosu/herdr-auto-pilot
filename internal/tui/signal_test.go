package tui

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

// headless lets a program start where there is no terminal: CI and `go test`
// have no /dev/tty, and bubbletea opens one unless input is explicitly
// disabled. The options under test (WithContext, WithoutSignalHandler) still
// come from run itself.
//
// It does narrow what these tests can see: with no input reader and no
// renderer, cancel-reader teardown and restoreTerminalState() never run, so
// the terminal-restore half of the shutdown fix is not covered here. That half
// is only observable against a real pty.
func headless() []tea.ProgramOption {
	return []tea.ProgramOption{tea.WithInput(nil), tea.WithoutRenderer()}
}

// runTestApp builds a store-backed App for driving Run end to end.
func runTestApp(t *testing.T) *frontend.App {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &frontend.App{
		Store:      st,
		Herdr:      &captureHerdr{},
		ConfigPath: filepath.Join(dir, "config.toml"),
		StateDir:   dir,
		Author:     "operator",
	}
}

// TestRunUnwindsOnContextCancel pins the wiring that makes signal handling
// work at all: Run passes its context to bubbletea, so cancelling it shuts the
// program down through bubbletea's own teardown — which restores the terminal
// and lets main's deferred store close and submit-retry drain run.
//
// bubbletea installs handlers for SIGINT and SIGTERM only. Every other signal
// main catches (notably SIGHUP, raised when the pane or ssh session hosting
// the TUI closes) reaches the TUI ONLY as a cancelled context, so a Run that
// ignores its context leaves those signals with Go's default disposition:
// the process dies where it stands and no cleanup runs.
func TestRunUnwindsOnContextCancel(t *testing.T) {
	app := runTestApp(t)
	ctx, cancel := context.WithCancel(context.Background())

	errc := make(chan error, 1)
	go func() { errc <- run(ctx, app, headless()...) }()

	// Let the program reach its event loop before pulling the context out.
	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case err := <-errc:
		// A cancelled context is a signal-driven exit, not a failure: main
		// must not print an error and exit non-zero when the operator simply
		// closes the pane.
		if err != nil {
			t.Errorf("Run after context cancel = %v, want a clean exit", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not return after its context was cancelled — the TUI ignores ctx, so SIGHUP cannot unwind it")
	}
}

// TestRunAlreadyCancelledExitsCleanly covers the race where the signal lands
// before the event loop is up: the context is already dead when the program
// starts. That still has to be a clean exit, not an error — closing the pane
// during startup is the same operator action as closing it a minute later.
func TestRunAlreadyCancelledExitsCleanly(t *testing.T) {
	app := runTestApp(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	errc := make(chan error, 1)
	go func() { errc <- run(ctx, app, headless()...) }()
	select {
	case err := <-errc:
		if err != nil {
			t.Errorf("Run with an already-cancelled context = %v, want a clean exit", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Run never returned for an already-cancelled context")
	}
}

// TestRunStillReportsRealFailures is the control for the two tests above: the
// nil they expect must come from the cancellation path specifically, not from
// Run having been taught to swallow errors. With a live context, a program
// that cannot start still reports why.
func TestRunStillReportsRealFailures(t *testing.T) {
	app := runTestApp(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errc := make(chan error, 1)
	// An input reader that only ever errors: bubbletea's cancel-reader setup
	// fails and Run returns that. Deliberately NOT WithInputTTY — `go test`
	// keeps its controlling terminal when run from a real shell, so that
	// variant passes in CI and hangs locally while eating the developer's
	// keystrokes.
	go func() { errc <- run(ctx, app, tea.WithoutRenderer(), tea.WithInput(errReader{})) }()
	select {
	case err := <-errc:
		if err == nil {
			t.Error("Run swallowed a genuine startup failure — a real error must still reach main")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Run never returned after a startup failure")
	}
}

// TestSignalDoesNotWedgeTheProgram pins tea.WithoutSignalHandler.
//
// Without it, a signal reaches bubbletea's own handler AND cancels the run
// context. The handler answers with a blocking send onto an internal channel
// that has no ctx escape, while bubbletea's shutdown waits on that goroutine
// unbounded — so whenever the event loop unwinds on ctx.Done() first, nothing
// is left to receive the message and the program never returns. Measured at
// roughly half of SIGINTs before the fix, so -count is what makes this
// deterministic; even a single run catches it often.
func TestSignalDoesNotWedgeTheProgram(t *testing.T) {
	app := runTestApp(t)
	// Catch the signal here as main does, so raising it cannot kill the test
	// binary — this asserts shutdown behavior, not signal disposition.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	errc := make(chan error, 1)
	go func() { errc <- run(ctx, app, headless()...) }()
	time.Sleep(200 * time.Millisecond)

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("raise SIGINT: %v", err)
	}
	select {
	case err := <-errc:
		if err != nil {
			t.Errorf("Run after SIGINT = %v, want a clean exit", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Run wedged after a signal — bubbletea's own signal handler is racing the context shutdown")
	}
}

// errReader fails every read, so a program using it cannot start.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("no input available") }
