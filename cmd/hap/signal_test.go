package main

import (
	"bufio"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestShutdownSignalsCancelInsteadOfKilling delivers each shutdown signal to
// this very test process, through the production wiring, and asserts the run
// context cancels.
//
// The assertion is deliberately self-enforcing: if a signal is dropped from
// shutdownSignals, this test does not report a failure — the test BINARY dies
// on the spot with Go's default disposition for it, which is exactly the
// production bug (`hap tui` killed mid-flight, no deferred store close,
// terminal left in raw mode). SIGHUP is the one that matters in practice: it
// arrives whenever the terminal hosting the TUI goes away — a closed herdr
// pane, a dropped ssh session.
func TestShutdownSignalsCancelInsteadOfKilling(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT} {
		t.Run(sig.String(), func(t *testing.T) {
			ctx, stop := shutdownContext()
			defer stop()

			if err := syscall.Kill(syscall.Getpid(), sig); err != nil {
				t.Fatalf("raise %v: %v", sig, err)
			}
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
				t.Fatalf("%v did not cancel the run context", sig)
			}
		})
	}
}

// signalChildEnv makes a re-executed test binary run the helper below instead
// of the suite. Signal DISPOSITION is process-wide and one-way here (the
// helper deliberately lets a signal kill it), so it cannot be asserted
// in-process — it needs a child.
const signalChildEnv = "HAP_TEST_SIGNAL_CHILD"

// TestSignalChildHelper is not a test: re-executed with signalChildEnv set, it
// installs the production wiring, reports readiness on stdout, and then waits.
// The parent signals it and inspects how it died.
func TestSignalChildHelper(t *testing.T) {
	if os.Getenv(signalChildEnv) == "" {
		t.Skip("helper process for TestSecondSignalStillTerminates")
	}
	ctx, stop := shutdownContext()
	defer stop()

	os.Stdout.WriteString("ready\n")
	<-ctx.Done()
	// First signal handled. Report that, then block: the parent's second
	// signal must now kill us, because shutdownContext released the handler.
	os.Stdout.WriteString("cancelled\n")
	select {}
}

// TestSecondSignalStillTerminates pins the escape hatch. Catching a signal
// means it no longer kills, so a process wedged somewhere that does not
// observe ctx would survive every signal short of SIGKILL — for an orphaned
// process whose terminal is already gone, that is a leak the operator cannot
// even see. The first signal must cancel gracefully; the second must kill.
//
// Both halves are load-bearing in opposite directions: release the handler too
// early and SIGHUP goes back to killing us instantly (the original bug), never
// release it and a wedged process lingers forever.
func TestSecondSignalStillTerminates(t *testing.T) {
	if testing.Short() {
		t.Skip("re-executes the test binary")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("cannot locate the test binary: %v", err)
	}

	cmd := exec.Command(exe, "-test.run=TestSignalChildHelper", "-test.v")
	cmd.Env = append(os.Environ(), signalChildEnv+"=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Exactly one Wait: a second concurrent call races the first and reports
	// nothing useful. It starts now, so the child is reaped however the test
	// ends, and closing the channel (rather than sending) lets both the
	// assertion and the cleanup read it.
	var waitErr error
	waited := make(chan struct{})
	go func() { waitErr = cmd.Wait(); close(waited) }()
	defer func() {
		_ = cmd.Process.Kill()
		<-waited
	}()

	// Read until the helper says it is armed, so the signal cannot land before
	// the handler is installed.
	sc := bufio.NewScanner(stdout)
	if !waitForLine(t, sc, "ready") {
		t.Fatal("helper never reported readiness")
	}

	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatalf("first SIGHUP: %v", err)
	}
	if !waitForLine(t, sc, "cancelled") {
		t.Fatal("first SIGHUP did not cancel the child's run context gracefully")
	}

	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatalf("second SIGHUP: %v", err)
	}

	select {
	case <-waited:
		exitErr, ok := waitErr.(*exec.ExitError)
		if !ok {
			t.Fatalf("child exited with %v, want death by signal", waitErr)
		}
		ws, ok := exitErr.Sys().(syscall.WaitStatus)
		if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGHUP {
			t.Errorf("child exit = %v, want killed by SIGHUP — the handler was not released after the first signal", exitErr.ProcessState)
		}
	case <-time.After(20 * time.Second):
		t.Error("child survived a second SIGHUP — a wedged process would be unkillable")
	}
}

// waitForLine reads scanner output until it sees want, so the parent never
// signals a child that is not listening yet. The scanner is shared across
// calls: each one resumes where the last stopped.
func waitForLine(t *testing.T, sc *bufio.Scanner, want string) bool {
	t.Helper()
	found := make(chan bool, 1)
	go func() {
		for sc.Scan() {
			if strings.TrimSpace(sc.Text()) == want {
				found <- true
				return
			}
		}
		found <- false
	}()
	select {
	case ok := <-found:
		return ok
	case <-time.After(30 * time.Second):
		return false
	}
}
