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
	// EmbedderDiag explains a non-ready embedder. Absent (nil) on older
	// daemons and whenever the engine offers no diagnostics — readers must
	// treat it as optional and fall back to the bare Embedder state.
	EmbedderDiag *EmbedderDiag `json:"embedder_diag,omitempty"`
}

// EmbedderDiag is the heartbeat's copy of embedder.Diagnostics. It lives here
// rather than being imported from internal/embedder so the health record stays
// a plain serializable struct (and daemonhealth keeps no CGO-tagged dependency).
//
// It exists because "degraded" on its own is not actionable: a model that is
// merely slower than the stall guard latches off exactly like a broken one, and
// only the timeout counters and the last error distinguish them.
type EmbedderDiag struct {
	ConsecutiveFailures int    `json:"consecutive_failures,omitempty"`
	MaxFailures         int    `json:"max_failures,omitempty"`
	Timeouts            int    `json:"timeouts,omitempty"`
	Failures            int    `json:"failures,omitempty"`
	LastError           string `json:"last_error,omitempty"`
	EmbedTimeoutMs      int    `json:"embed_timeout_ms,omitempty"`
	WarmTimeoutMs       int    `json:"warm_timeout_ms,omitempty"`
	// TimeoutBound marks the case where every failure was a stall-guard
	// expiry — i.e. raising the budgets is the remedy, not disabling
	// embeddings.
	TimeoutBound bool `json:"timeout_bound,omitempty"`
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
	if h.Embedder != EmbedderDegraded {
		return string(h.Embedder)
	}
	// A purely timeout-driven degrade is a tuning problem, not a broken
	// embedder: pointing that operator at `embedding.disabled` would throw away
	// semantic matching for a model that only needed a longer budget.
	if d := h.EmbedderDiag; d != nil && d.TimeoutBound {
		return fmt.Sprintf("%s (every embed hit the stall guard; raise `embedding.embed_timeout_ms` (now %dms) / `embedding.warm_timeout_ms` (now %dms) — changing [embedding] config rebuilds the embedder and clears this)",
			h.Embedder, d.EmbedTimeoutMs, d.WarmTimeoutMs)
	}
	return fmt.Sprintf("%s (embedder fell back to text matching; run: hap config set embedding.disabled true to silence)", h.Embedder)
}

// EmbedderDiagLines renders the diagnostic evidence behind a degraded embedder
// as indented detail lines (empty when there is nothing to add), so `hap status`
// and the TUI show the same explanation.
func (h Health) EmbedderDiagLines() []string {
	d := h.EmbedderDiag
	// Nothing has gone wrong: stay silent. The budgets are always populated
	// (they are the resolved defaults), so printing them unconditionally would
	// add noise to every healthy `hap status` forever.
	if d == nil || (d.Failures == 0 && d.Timeouts == 0) {
		return nil
	}
	lines := []string{
		fmt.Sprintf("failures: %d (%d timeouts), latch at %d consecutive",
			d.Failures, d.Timeouts, d.MaxFailures),
		fmt.Sprintf("budgets: embed %dms, warm %dms", d.EmbedTimeoutMs, d.WarmTimeoutMs),
	}
	if d.LastError != "" {
		lines = append(lines, "last error: "+d.LastError)
	}
	return lines
}
