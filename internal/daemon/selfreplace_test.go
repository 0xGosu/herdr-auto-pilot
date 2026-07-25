package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// writeExe creates a file with the execute bit, standing in for an installed
// hap binary.
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

// selfReplaceDaemon builds the minimum Daemon checkOwnBinary needs. The full
// harness is deliberately avoided: this decision reads only the recorded exe
// path and the two injected hooks, and building a whole daemon would couple
// the test to everything else that has to be wired for Run.
func selfReplaceDaemon(exePath string, resolve func() (string, error), handOff func(string) error) *Daemon {
	return &Daemon{
		opt:     Options{ResolveSelf: resolve, HandOff: handOff, Clock: ports.SystemClock{}},
		exePath: exePath,
	}
}

// The upgrade case: our binary is gone, a new one is installed elsewhere. The
// daemon must start the successor and step aside — every child it would spawn
// from the removed path (the MCP server the LLM CLI launches, the embed
// worker) fails, so staying up serves nothing.
func TestCheckOwnBinaryHandsOffToReplacement(t *testing.T) {
	dir := t.TempDir()
	removed := filepath.Join(dir, "old", "bin", "hap")
	successor := writeExe(t, filepath.Join(dir, "new", "bin", "hap"))

	var handedTo []string
	d := selfReplaceDaemon(removed,
		func() (string, error) { return successor, nil },
		func(exe string) error { handedTo = append(handedTo, exe); return nil })

	if !d.checkOwnBinary() {
		t.Fatal("checkOwnBinary = false; a replaced binary must end the loop")
	}
	if len(handedTo) != 1 || handedTo[0] != successor {
		t.Fatalf("handed off to %v, want exactly [%s]", handedTo, successor)
	}
	if d.binaryReplaced.Load() {
		t.Error("a successful handoff is not a degraded state; health must not flag it")
	}
	// The heartbeat keeps ticking while the spawn is in flight; a second
	// handoff would race the first for the daemon lock.
	if d.checkOwnBinary() {
		t.Error("checkOwnBinary fired twice; the handoff must be one-shot")
	}
	if len(handedTo) != 1 {
		t.Fatalf("handed off %d times, want exactly 1", len(handedTo))
	}
}

// With no successor to hand to, exiting would leave the herd unmonitored. Stay
// up, degraded, and let health say so.
func TestCheckOwnBinaryWithoutSuccessorStaysUpAndFlags(t *testing.T) {
	dir := t.TempDir()
	removed := filepath.Join(dir, "old", "bin", "hap")

	handoffs := 0
	d := selfReplaceDaemon(removed,
		func() (string, error) { return "", fmt.Errorf("nothing found") },
		func(string) error { handoffs++; return nil })

	if d.checkOwnBinary() {
		t.Fatal("checkOwnBinary = true with no successor; the daemon must keep running")
	}
	if handoffs != 0 {
		t.Errorf("handoff attempted %d times with no successor", handoffs)
	}
	if !d.binaryReplaced.Load() {
		t.Error("health must flag a removed binary so `hap status` can report it")
	}
}

// A successor that will not start is, for now, the same situation as none at
// all: stay up and report it.
func TestCheckOwnBinaryStaysUpWhenHandoffFails(t *testing.T) {
	dir := t.TempDir()
	removed := filepath.Join(dir, "old", "bin", "hap")
	successor := writeExe(t, filepath.Join(dir, "new", "bin", "hap"))

	d := selfReplaceDaemon(removed,
		func() (string, error) { return successor, nil },
		func(string) error { return fmt.Errorf("spawn refused") })

	if d.checkOwnBinary() {
		t.Fatal("checkOwnBinary = true after a failed handoff; nothing would be monitoring")
	}
	if !d.binaryReplaced.Load() {
		t.Error("a failed handoff must be reported as degraded")
	}
}

// A failure must not be terminal. The successor may simply have been mid-write
// when we first tried (install.sh unpacks while the daemon runs), so the
// attempt is retried — but paced, not once per 10s heartbeat, which would
// spawn-storm a genuinely broken successor.
func TestCheckOwnBinaryRetriesAFailedHandoffOnceThePaceElapses(t *testing.T) {
	dir := t.TempDir()
	removed := filepath.Join(dir, "old", "bin", "hap")
	successor := writeExe(t, filepath.Join(dir, "new", "bin", "hap"))

	attempts := 0
	failNext := true
	d := selfReplaceDaemon(removed,
		func() (string, error) { return successor, nil },
		func(string) error {
			attempts++
			if failNext {
				return fmt.Errorf("spawn refused")
			}
			return nil
		})

	if d.checkOwnBinary() || attempts != 1 {
		t.Fatalf("first attempt: returned true or attempts=%d, want false and 1", attempts)
	}
	// An immediate second heartbeat must NOT retry.
	if d.checkOwnBinary() || attempts != 1 {
		t.Fatalf("retry was not paced: attempts=%d, want still 1", attempts)
	}
	// Once the interval has elapsed it retries, and succeeds this time.
	d.lastHandoff.Store(time.Now().Add(-2 * handoffRetryInterval).UnixNano())
	failNext = false
	if !d.checkOwnBinary() {
		t.Fatal("checkOwnBinary = false; the retry succeeded and should end the loop")
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

// The handoff is the first path that returns from Run WITHOUT the caller's
// context being cancelled. Everything Run started must still unwind: the event
// subscriber loops on reconnect backoff until its context is done, so a Run
// that returned while its goroutines rooted at the (still live) parent context
// hung forever in the background drain — holding the daemon lock the successor
// was waiting for, which left the herd with no daemon at all. Verified against
// a real daemon before the fix.
func TestRunReturnsPromptlyAfterHandoff(t *testing.T) {
	dir := t.TempDir()
	// The daemon must start from a binary that exists and then lose it, so the
	// heartbeat check sees a replacement rather than a path that was never there.
	removed := writeExe(t, filepath.Join(dir, "old", "bin", "hap"))
	successor := writeExe(t, filepath.Join(dir, "new", "bin", "hap"))

	h := newHarness(t, "")
	// Take the harness's daemon off its own Run before re-running it here with
	// the self-replacement hooks wired in.
	h.stop()

	handedOff := make(chan string, 1)
	// Signalled by the hook rather than read off d.exePath: that field is
	// written by Run's goroutine, so polling it from here would be a data race.
	identified := make(chan struct{}, 1)
	d, err := New(Options{
		ConfigPath: h.cfgPath,
		Store:      h.store,
		Herdr:      h.herdr,
		Events:     &fakeEvents{ch: make(chan domain.AgentTransition, 1)},
		StateDir:   t.TempDir(),
		ResolveSelf: func() (string, error) {
			// The first call is Run recording our identity; later calls are the
			// heartbeat looking for a successor.
			if _, err := os.Stat(removed); err == nil {
				select {
				case identified <- struct{}{}:
				default:
				}
				return removed, nil
			}
			return successor, nil
		},
		HandOff: func(exe string) error { handedOff <- exe; return nil },
	})
	if err != nil {
		t.Fatal(err)
	}

	// A context that is never cancelled: the point of the test is that Run
	// tears itself down without one.
	done := make(chan error, 1)
	go func() { done <- d.Run(context.Background()) }()

	select {
	case <-identified:
	case <-time.After(5 * time.Second):
		t.Fatal("Run never resolved its own binary")
	}
	if err := os.Remove(removed); err != nil {
		t.Fatal(err)
	}

	select {
	case exe := <-handedOff:
		if exe != successor {
			t.Fatalf("handed off to %q, want %q", exe, successor)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no handoff within 30s of the binary being removed")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after handoff = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after handing off — its background goroutines never unwound")
	}
}

func TestCheckOwnBinaryNoOpCases(t *testing.T) {
	dir := t.TempDir()
	live := writeExe(t, filepath.Join(dir, "bin", "hap"))
	successor := writeExe(t, filepath.Join(dir, "new", "bin", "hap"))
	removed := filepath.Join(dir, "old", "bin", "hap")

	tests := []struct {
		name    string
		exePath string
		resolve func() (string, error)
		handOff func(string) error
	}{
		{
			name:    "binary still on disk",
			exePath: live,
			resolve: func() (string, error) { return successor, nil },
			handOff: func(string) error { return nil },
		},
		{
			name: "exe path unknown",
			// Resolution failed at startup, so there is nothing to compare
			// against; guessing would restart a healthy daemon.
			exePath: "",
			resolve: func() (string, error) { return successor, nil },
			handOff: func(string) error { return nil },
		},
		{
			// Still flagged for health (see the assertion below); it just
			// cannot start the replacement itself.
			name:    "no handoff configured",
			exePath: removed,
			resolve: func() (string, error) { return successor, nil },
			handOff: nil,
		},
		{
			name: "resolver returns our own removed path",
			// Nothing replaced us — the binary is simply gone. Handing off to
			// the same dead path would spawn nothing.
			exePath: removed,
			resolve: func() (string, error) { return removed, nil },
			handOff: func(string) error { return nil },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := selfReplaceDaemon(tt.exePath, tt.resolve, tt.handOff)
			if d.checkOwnBinary() {
				t.Fatal("checkOwnBinary = true; the daemon must keep running")
			}
		})
	}
}

// Noticing a removed binary costs one Stat, so health reports it even when
// this daemon has no way to start a replacement — an operator reading
// `hap status` deserves the answer either way.
func TestCheckOwnBinaryFlagsHealthWithoutAHandoffHook(t *testing.T) {
	dir := t.TempDir()
	removed := filepath.Join(dir, "old", "bin", "hap")
	successor := writeExe(t, filepath.Join(dir, "new", "bin", "hap"))

	d := selfReplaceDaemon(removed, func() (string, error) { return successor, nil }, nil)
	if d.checkOwnBinary() {
		t.Fatal("checkOwnBinary = true with no handoff hook")
	}
	if !d.binaryReplaced.Load() {
		t.Error("health must flag the removed binary even when the takeover is not wired")
	}
}
