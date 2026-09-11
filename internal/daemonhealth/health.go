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
	// ExePath is the binary this daemon is running as. A plugin upgrade
	// installs the new release beside the old one and unlinks it, so this
	// path can stop existing under a perfectly live process — at which point
	// every child it spawns from that path (the MCP server the LLM CLI
	// launches, the embed worker) fails. Absent on older daemons.
	ExePath string `json:"exe_path,omitempty"`
	// BinaryReplaced records that ExePath has gone away and no successor
	// could be found to hand off to. The daemon keeps running (something
	// monitoring beats nothing), but it is degraded in a way only
	// `hap daemon --ensure` from a fresh binary can fix.
	BinaryReplaced bool `json:"binary_replaced,omitempty"`
	// FleetSync is the shared database's sync state under the turso engine;
	// absent under the local engine and on older daemons.
	FleetSync *FleetSyncHealth `json:"fleet_sync,omitempty"`
	// Orchestrator is the full-self-prompting orchestrator session's trouble:
	// a start that keeps failing, or a brief held by a claude prompt only the
	// operator should answer. Absent when the feature is off or all is well
	// (and on older daemons) — its failures otherwise live only in the log.
	Orchestrator *OrchestratorHealth `json:"orchestrator,omitempty"`
}

// OrchestratorHealth is the heartbeat's copy of the orchestrator's state.
type OrchestratorHealth struct {
	// LastError is why the last attempt to start, adopt or brief the session
	// failed; empty once one succeeds.
	LastError   string    `json:"last_error,omitempty"`
	LastErrorAt time.Time `json:"last_error_at,omitempty"`
	// Failures counts failed attempts in a row; RetryAt is when the daemon
	// tries again (zero: at the next sweep).
	Failures int       `json:"failures,omitempty"`
	RetryAt  time.Time `json:"retry_at,omitempty"`
	// Waiting: the session is up but its brief is held because claude shows a
	// prompt (trusting a new directory, say) the daemon will not answer.
	Waiting   bool   `json:"waiting,omitempty"`
	Workspace string `json:"workspace,omitempty"`
}

// Failing reports that the last attempt failed.
func (o *OrchestratorHealth) Failing() bool { return o != nil && o.LastError != "" }

// Line is the status-page rendering, or "" when there is nothing to report.
func (o *OrchestratorHealth) Line(now time.Time) string {
	switch {
	case o == nil:
		return ""
	case o.LastError != "":
		next := "at the next sweep"
		if o.RetryAt.After(now) {
			next = "in " + o.RetryAt.Sub(now).Round(time.Second).String()
		}
		return fmt.Sprintf("NOT RUNNING — %s (%d failed attempt(s); next try %s)", o.LastError, o.Failures, next)
	case o.Waiting:
		return fmt.Sprintf("waiting on a claude prompt in workspace %s — answer it once and hap sends the brief", o.Workspace)
	}
	return ""
}

// FleetSyncIsolatedAfter is how long a node may go without a SUCCESSFUL pull
// or push before its degradation stops being weather and starts being
// isolation — the point at which the TUI raises it from a warning to an error.
//
// A single failed tick is not it. Under the default 15s pull interval this is
// twenty of them, which no transient reaches, and it is the same order as the
// window the automatic recovery waits out: an operator who sees the error
// banner is seeing a state the daemon has already tried to fix itself.
//
// It is deliberately a CLOCK, not a boolean on LastError. A banner that fires
// on the first blip is a banner nobody reads by the end of the week.
const FleetSyncIsolatedAfter = 5 * time.Minute

// FleetSyncHealth is the heartbeat's copy of the fleet sync loop's state.
type FleetSyncHealth struct {
	Engine string `json:"engine"`
	// Bootstrapped is false while the first start is still waiting for the
	// remote to hand over the initial database.
	Bootstrapped bool      `json:"bootstrapped"`
	LastPullAt   time.Time `json:"last_pull_at,omitempty"`
	LastPushAt   time.Time `json:"last_push_at,omitempty"`
	PendingOps   int64     `json:"pending_ops,omitempty"`
	Revision     string    `json:"revision,omitempty"`
	LastError    string    `json:"last_error,omitempty"`
	LastErrorAt  time.Time `json:"last_error_at,omitempty"`
	// ConsecutiveFailures counts sync operations that have failed in a row
	// across BOTH directions, reset by any success. A node that pulls fine
	// and cannot push is just as isolated as one that can do neither, so a
	// counter that tracked only pulls would never see it.
	ConsecutiveFailures int `json:"consecutive_failures,omitempty"`
	// FirstFailureAt anchors the CURRENT outage — the first failure since the
	// last success, or since start when there has never been one. It is what
	// makes "degraded for 12m" answerable on a daemon that has never synced,
	// where LastPullAt and LastPushAt are both zero.
	FirstFailureAt time.Time `json:"first_failure_at,omitempty"`
	// FDSoft, FDHard, FDOpen and FDExhausted are the descriptor budget as it
	// stood at the LAST FAILURE — instrumentation, carried so the next
	// occurrence is a one-command diagnosis rather than another round of
	// guessing (see internal/fdprobe). FDOpenKnown separates "none open"
	// from "this platform cannot count them"; FDExhausted is the only one
	// that is proof, and it is available everywhere.
	FDSoft      uint64 `json:"fd_soft,omitempty"`
	FDHard      uint64 `json:"fd_hard,omitempty"`
	FDOpen      int    `json:"fd_open,omitempty"`
	FDOpenKnown bool   `json:"fd_open_known,omitempty"`
	FDExhausted bool   `json:"fd_exhausted,omitempty"`
	// RecoveredAt records when this daemon last handed the herd to a fresh
	// process to clear a wedged sync engine, and RecoveryLatched that it may
	// not do so again until it has seen one success. Both are display-only
	// here; the authority is the marker file the daemon keeps.
	RecoveredAt     time.Time `json:"recovered_at,omitempty"`
	RecoveryLatched bool      `json:"recovery_latched,omitempty"`
}

// Degraded reports that the last sync operation failed, at any age.
func (f *FleetSyncHealth) Degraded() bool {
	return f != nil && (!f.Bootstrapped || f.LastError != "")
}

// LastProgressAt is the most recent SUCCESSFUL sync in either direction, zero
// when there has never been one.
func (f *FleetSyncHealth) LastProgressAt() time.Time {
	if f == nil {
		return time.Time{}
	}
	if f.LastPushAt.After(f.LastPullAt) {
		return f.LastPushAt
	}
	return f.LastPullAt
}

// IsolatedFor is how long this node has gone without a successful sync while
// failing, or 0 when it is not degraded at all. It measures from the last
// success, falling back to the outage's own start when there has never been
// one — which is the ONLY anchor a daemon that has never synced has.
func (f *FleetSyncHealth) IsolatedFor(now time.Time) time.Duration {
	if !f.Degraded() {
		return 0
	}
	since := f.LastProgressAt()
	if since.IsZero() {
		since = f.FirstFailureAt
	}
	if since.IsZero() {
		return 0
	}
	if d := now.Sub(since); d > 0 {
		return d
	}
	return 0
}

// Isolated reports that the degradation has lasted long enough to mean this
// machine's escalations are not reaching the fleet and the fleet's are not
// reaching it.
//
// It REQUIRES Bootstrapped, and that is not a detail: isolation means this node
// was part of a fleet and has been cut off, whereas an unbootstrapped one never
// joined — it has no database at all. Both are Degraded and both accumulate an
// IsolatedFor (the bootstrap retry is unbounded and records its own
// FirstFailureAt), so without this clause a node stuck on a rejected token
// reads as isolated and every reader that asks only this question tells the
// operator their peers' escalations are not reaching them — pointing at the
// sync engine when the answer is the URL and the token. `hap status` did
// exactly that, because it asks with a bare `if` rather than an ordered switch.
// The bootstrap case carries its own escalation in frontend.DaemonHealth, so
// nothing is lost by narrowing this.
func (f *FleetSyncHealth) Isolated(now time.Time) bool {
	return f != nil && f.Bootstrapped && f.IsolatedFor(now) >= FleetSyncIsolatedAfter
}

// Line renders the state as the one line `hap status` prints.
func (f *FleetSyncHealth) Line(now time.Time) string {
	if f == nil {
		return ""
	}
	ago := func(t time.Time) string {
		if t.IsZero() {
			return "never"
		}
		return now.Sub(t).Round(time.Second).String() + " ago"
	}
	if !f.Bootstrapped {
		return fmt.Sprintf("%s — BOOTSTRAP PENDING: %s", f.Engine, f.LastError)
	}
	if f.LastError != "" {
		state := "DEGRADED"
		if f.Isolated(now) {
			state = "ISOLATED"
		}
		return fmt.Sprintf("%s — %s for %s (%d consecutive failures): %s (last pull %s, last push %s, %d unpushed)",
			f.Engine, state, f.IsolatedFor(now).Round(time.Second), f.ConsecutiveFailures, f.LastError,
			ago(f.LastPullAt), ago(f.LastPushAt), f.PendingOps)
	}
	return fmt.Sprintf("%s — ok (last pull %s, last push %s, %d unpushed)",
		f.Engine, ago(f.LastPullAt), ago(f.LastPushAt), f.PendingOps)
}

// DiagLines is the evidence behind a degraded sync, as indented detail lines:
// the descriptor budget at the last failure, and whether an automatic recovery
// has already been spent. Empty when the sync is healthy — there is nothing to
// explain.
func (f *FleetSyncHealth) DiagLines(now time.Time) []string {
	if !f.Degraded() {
		return nil
	}
	var out []string
	if f.FDExhausted {
		out = append(out, fmt.Sprintf("file descriptors: EXHAUSTED at the last failure (limit %d soft / %d hard) — raise it with `ulimit -n` before starting the daemon", f.FDSoft, f.FDHard))
	} else if f.FDSoft > 0 {
		open := "unavailable on this platform"
		if f.FDOpenKnown {
			open = fmt.Sprintf("%d open", f.FDOpen)
		}
		out = append(out, fmt.Sprintf("file descriptors: %s, limit %d soft / %d hard (not exhausted)", open, f.FDSoft, f.FDHard))
	}
	switch {
	case f.RecoveryLatched && !f.RecoveredAt.IsZero():
		out = append(out, fmt.Sprintf("automatic recovery: already restarted %s ago and it did not help — this needs a human",
			now.Sub(f.RecoveredAt).Round(time.Second)))
	case f.RecoveryLatched:
		out = append(out, "automatic recovery: spent (this daemon was started by one and has not synced since)")
	}
	return out
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
