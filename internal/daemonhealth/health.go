// Package daemonhealth persists a small heartbeat/health record that the live
// daemon updates while it runs, so out-of-process commands (`hap status`, the
// TUI) can tell a healthy, progressing daemon from a hung or degraded one.
//
// The flock in internal/daemonlock only answers "does some process hold the
// lock" — the OS releases it the instant the holder dies, so a truly dead pid
// never reads as running. What flock CANNOT reveal is a daemon that is alive
// but wedged (no forward progress) or one whose embedder has fallen back to
// text matching. A periodically-refreshed heartbeat file closes that gap: a
// stale heartbeat under a held lock means "hung", and the embedder field
// surfaces the soft-degraded state. (A hard native abort — the #60 SIGABRT —
// kills the process before it can write anything, so that case is caught by
// the captured stderr log and the restart tracker, not this file.)
package daemonhealth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// HeartbeatInterval is how often the running daemon refreshes its heartbeat.
const HeartbeatInterval = 10 * time.Second

// StaleAfter is how long a heartbeat may age before `hap status` treats a
// lock-holding daemon as hung. Three missed beats — tolerant of a brief GC or
// scheduling hiccup, still well under a human's patience.
const StaleAfter = 35 * time.Second

// EmbedderState is the daemon's semantic-matching health, as last written.
type EmbedderState string

const (
	// EmbedderReady means the model loaded and the match index is serving.
	EmbedderReady EmbedderState = "ready"
	// EmbedderDegraded means embed calls latched into failure — matching has
	// fallen back to BM25/exact text (the SOFT degrade; a hard abort never
	// gets to write this).
	EmbedderDegraded EmbedderState = "degraded"
	// EmbedderDisabled means semantic matching is off by config.
	EmbedderDisabled EmbedderState = "disabled"
	// EmbedderStarting means the background init has not reported yet.
	EmbedderStarting EmbedderState = "starting"
)

// Health is the persisted heartbeat record. It is written atomically by the
// daemon and read (best-effort, may be absent) by status/TUI.
type Health struct {
	PID         int           `json:"pid"`
	Version     string        `json:"version"`
	StartedAt   time.Time     `json:"started_at"`
	HeartbeatAt time.Time     `json:"heartbeat_at"`
	Embedder    EmbedderState `json:"embedder"`
}

// FileName is the health file's basename inside the state dir.
const FileName = "daemon.health.json"

// Path returns the health file path for a state directory.
func Path(stateDir string) string { return filepath.Join(stateDir, FileName) }

// Write atomically persists h to the state dir (temp file + rename, matching
// config.Save), so a concurrent reader never sees a torn record.
func Write(stateDir string, h Health) error {
	data, err := json.Marshal(h)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(stateDir, ".daemon-health-*.json")
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

// Read returns the persisted health record and whether one was found. A
// missing file yields ok=false with no error; a malformed file also yields
// ok=false (a corrupt heartbeat is treated as "no signal", never fatal).
func Read(stateDir string) (Health, bool) {
	data, err := os.ReadFile(Path(stateDir))
	if err != nil {
		return Health{}, false
	}
	var h Health
	if err := json.Unmarshal(data, &h); err != nil {
		return Health{}, false
	}
	return h, true
}

// Remove deletes the health file (best-effort, on clean shutdown). A missing
// file is not an error.
func Remove(stateDir string) error {
	err := os.Remove(Path(stateDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// Stale reports whether the heartbeat is older than StaleAfter relative to
// now. A zero heartbeat (never written) is stale.
func (h Health) Stale(now time.Time) bool {
	if h.HeartbeatAt.IsZero() {
		return true
	}
	return now.Sub(h.HeartbeatAt) > StaleAfter
}

// Age returns how long ago the heartbeat was written, floored at zero.
func (h Health) Age(now time.Time) time.Duration {
	d := now.Sub(h.HeartbeatAt)
	if d < 0 {
		return 0
	}
	return d
}

// EmbedderNote returns a human-facing suffix describing a non-ready embedder,
// or "" when ready/unknown (nothing to warn about).
func (h Health) EmbedderNote() string {
	switch h.Embedder {
	case EmbedderDegraded:
		return fmt.Sprintf("%s (embedder fell back to text matching; run: hap config set embedding.disabled true to silence)", h.Embedder)
	default:
		return string(h.Embedder)
	}
}
