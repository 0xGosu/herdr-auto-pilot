// Package crashguard turns a daemon crash-loop into a graceful, self-limiting
// degrade instead of an endless respawn storm.
//
// The motivating failure (#60): a native abort in the embedder (llama.cpp
// GGML_ASSERT → SIGABRT) kills the daemon during startup, herdr's --ensure
// hook respawns it, and it dies again — an accelerating loop that never
// recovers and surfaces nothing. A native abort cannot be caught in Go, so the
// only lever is out-of-process state: record each boot, and when boots cluster
// too tightly, mitigate.
//
// The state machine (all decided in Evaluate, a pure function; the daemon does
// the I/O around it):
//
//   - Each daemon boot appends a timestamp, pruned to a rolling Window.
//   - Threshold boots within Window ⇒ crash-loop. First trip auto-disables the
//     embedder (boot with BM25 fallback) — semantic matching is designed to
//     degrade, and the embedder is the usual culprit. The disable LATCHES.
//   - Still looping with the embedder already off ⇒ the crash is not the
//     embedder; give up (stop starting) so the storm ends.
//   - Latches clear only when the [embedding] config digest changes (operator
//     intervention) — never automatically, so a restart can't silently
//     re-enter the same loop. Surviving the Window clears the boot history
//     (the loop is broken) but keeps the latch.
package crashguard

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Window is the rolling span over which boots are counted, and Threshold is
// how many boots within it constitute a crash-loop. Tuned to catch #60's
// accelerating loop (measured gaps 49s→32s→20s→11s) before it fully dies,
// without tripping on a couple of legitimate restarts.
const (
	Window    = 90 * time.Second
	Threshold = 3
)

// FileName is the crashguard state file's basename inside the state dir.
const FileName = "daemon.crashguard.json"

// State is the persisted crash-loop record.
type State struct {
	// Starts are recent daemon boot timestamps, pruned to Window.
	Starts []time.Time `json:"starts"`
	// EmbeddingOff latches the auto-fallback: boot with the embedder disabled
	// until the config digest changes.
	EmbeddingOff bool `json:"embedding_off"`
	// GaveUp latches the hard stop: still looping even with the embedder off,
	// so stop starting entirely until the config digest changes.
	GaveUp bool `json:"gave_up"`
	// Reason is the human explanation for the current latch (for status).
	Reason string `json:"reason,omitempty"`
	// SetAt is when the current latch tripped.
	SetAt time.Time `json:"set_at,omitempty"`
	// ConfigDigest fingerprints the [embedding] config as of the last boot; a
	// change clears the latches (the operator changed something).
	ConfigDigest string `json:"config_digest,omitempty"`
}

// Decision is what a booting daemon should do, returned by Evaluate.
type Decision struct {
	// DisableEmbedding forces the embedder off for this run (auto-fallback).
	DisableEmbedding bool
	// GiveUp means do not run at all — the loop is unrecoverable by degrading.
	GiveUp bool
	// Reason explains a non-normal decision (for logs/status).
	Reason string
}

// Evaluate records a boot at now for the given [embedding] config digest and
// returns the updated state plus the boot decision. Pure: the caller reads the
// prior state, calls this, and persists the result.
func Evaluate(st State, now time.Time, digest string) (State, Decision) {
	// An [embedding] config change since the last boot is operator
	// intervention: clear every latch and the boot history, giving semantic
	// matching a clean attempt.
	if st.ConfigDigest != "" && digest != st.ConfigDigest {
		st = State{ConfigDigest: digest}
	}
	st.ConfigDigest = digest

	st.Starts = append(pruneStarts(st.Starts, now), now)
	looping := len(st.Starts) >= Threshold

	switch {
	case st.GaveUp:
		// Latched hard stop persists until the config digest changes (handled
		// above); the daemon must not run.
		return st, Decision{GiveUp: true, Reason: st.Reason}
	case st.EmbeddingOff:
		if looping {
			// Disabling the embedder did not stop the loop — the crash is
			// elsewhere; stop starting.
			st.GaveUp = true
			st.SetAt = now
			st.Reason = fmt.Sprintf("crash-looping even with the embedder disabled (%d starts within %s); not restarting until the [embedding] config changes", len(st.Starts), Window)
			return st, Decision{GiveUp: true, Reason: st.Reason}
		}
		return st, Decision{DisableEmbedding: true, Reason: st.Reason}
	default:
		if looping {
			st.EmbeddingOff = true
			st.SetAt = now
			st.Reason = fmt.Sprintf("semantic matching auto-disabled after %d daemon starts within %s (suspected embedder crash-loop); change the [embedding] config to re-enable", len(st.Starts), Window)
			// Reset the boot history to just this boot: escalating to give-up
			// must require a FRESH cluster of boots AFTER the embedder is off
			// (genuinely "still looping"), not the same cluster that tripped
			// the auto-disable. Otherwise a single clean restart or upgrade
			// landing in the window would jump straight to a full stop.
			st.Starts = []time.Time{now}
			return st, Decision{DisableEmbedding: true, Reason: st.Reason}
		}
		return st, Decision{}
	}
}

// Survived clears the boot history once a daemon has stayed up past Window
// (the loop is broken), keeping any latch. Returns the state to persist and
// whether anything changed.
func (st State) Survived() (State, bool) {
	if len(st.Starts) == 0 {
		return st, false
	}
	st.Starts = nil
	return st, true
}

// EmbeddingSuppressed reports whether the auto-fallback latch disables the
// embedder for the given [embedding] config digest. When the digest differs
// from when the latch tripped, the operator changed the config: it returns
// changed=true with the cleared state to persist (so a live reload, not just a
// restart, re-enables semantic matching). Used by the daemon's embedder
// factory on both first build and reload.
func EmbeddingSuppressed(st State, digest string) (suppressed bool, cleared State, changed bool) {
	if !st.EmbeddingOff {
		return false, st, false
	}
	if st.ConfigDigest != "" && digest != st.ConfigDigest {
		return false, State{ConfigDigest: digest}, true
	}
	return true, st, false
}

// SpawnBlocked reports whether an --ensure should decline to start a daemon: a
// latched give-up that the current config digest has not cleared. When the
// digest differs it returns the cleared state to persist (the operator changed
// config; let the next start try again).
func SpawnBlocked(st State, digest string) (blocked bool, cleared State, reason string) {
	if !st.GaveUp {
		return false, st, ""
	}
	if st.ConfigDigest != "" && digest != st.ConfigDigest {
		return false, State{ConfigDigest: digest}, ""
	}
	return true, st, st.Reason
}

func pruneStarts(starts []time.Time, now time.Time) []time.Time {
	cutoff := now.Add(-Window)
	kept := starts[:0:0]
	for _, t := range starts {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	return kept
}

// Path returns the crashguard state file path for a state directory.
func Path(stateDir string) string { return filepath.Join(stateDir, FileName) }

// Read returns the persisted state and whether one was found. Missing or
// malformed yields ok=false with no error — a corrupt guard file must never
// block the daemon; it just resets the loop tracking.
func Read(stateDir string) (State, bool) {
	data, err := os.ReadFile(Path(stateDir))
	if err != nil {
		return State{}, false
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return State{}, false
	}
	return st, true
}

// Write atomically persists st (temp + rename).
func Write(stateDir string, st State) error {
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(stateDir, ".crashguard-*.json")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, Path(stateDir))
}

// Remove deletes the crashguard state (best-effort). A missing file is fine.
func Remove(stateDir string) error {
	err := os.Remove(Path(stateDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
