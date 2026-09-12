package frontend

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/buildinfo"
	"github.com/0xGosu/herdr-auto-pilot/internal/crashguard"
	"github.com/0xGosu/herdr-auto-pilot/internal/daemonhealth"
)

// maxStderrTailBytes bounds how much of the captured daemon stderr a front-end
// pulls into memory for the post-mortem detail view — a native abort dumps a
// backtrace, but the reason line is near the end, so the tail is what matters.
const maxStderrTailBytes = 16 << 10 // 16 KiB

// DaemonSeverity ranks daemon health for front-ends that surface a single
// banner (the TUI) or an exit code (the CLI).
type DaemonSeverity int

const (
	// DaemonOK: running and progressing, or cleanly stopped — nothing to show.
	DaemonOK DaemonSeverity = iota
	// DaemonWarn: working but degraded (BM25 fallback, or an older binary).
	DaemonWarn
	// DaemonError: hung, or the crash-loop breaker stopped it — needs attention.
	DaemonError
)

// DaemonHealth is a front-end-agnostic assessment of the daemon, combining the
// lock (DaemonInfo), the heartbeat (daemonhealth), and the crash-loop breaker
// (crashguard). cli status and the TUI banner both classify from this so they
// can never disagree. Absent state files degrade gracefully — a field stays
// false; it never errors.
type DaemonHealth struct {
	Running      bool
	PID          int
	Version      string
	VersionStale bool // running a different binary than this one
	// BinaryReplaced: the running daemon's own executable was removed (a
	// plugin upgrade installs the new release elsewhere) and it found no
	// successor to hand the herd to. It is alive and beating, but every child
	// it spawns by path — the MCP server the LLM CLI launches, the embed
	// worker — fails, so consults come back empty.
	BinaryReplaced bool
	// Hung: a held lock with a stale heartbeat — alive but not progressing.
	Hung         bool
	HeartbeatAge time.Duration
	// GaveUp: the crash-loop breaker stopped restarting the daemon.
	GaveUp bool
	// CrashLooping: the daemon is down with a recent cluster of boots — it is
	// crashing and being respawned, but the breaker has not yet given up. This
	// is the primary #60 symptom (a dead daemon that otherwise looks quiet).
	CrashLooping bool
	// RecentRestarts is how many daemon boots fall within the breaker's window.
	RecentRestarts int
	// EmbeddingAutoDisabled: the breaker forced the embedder off (BM25 fallback).
	EmbeddingAutoDisabled bool
	// EmbedderDegraded: the running embedder soft-degraded (embed calls latched
	// to text matching).
	EmbedderDegraded bool
	// EmbedderNote is the daemonhealth remediation text for a degraded embedder
	// (single source of the exact wording, incl. the `hap config set …` hint).
	EmbedderNote string
	// EmbedderDiagLines is the evidence behind the embedder's state — failure
	// and timeout counts, the effective stall-guard budgets, the last error —
	// rendered as indented detail lines. Present whenever the daemon reported
	// diagnostics and something has failed, degraded or not: a run of timeouts
	// short of the latch is the early warning that the budgets are too tight.
	EmbedderDiagLines []string
	// FleetSyncLine describes the shared database's sync state under the
	// turso engine ("" under the local engine).
	FleetSyncLine string
	// FleetSyncBootstrapping: the daemon is still waiting for Turso Cloud to
	// hand over the initial database and has not started monitoring anything.
	// It is a DIFFERENT state from a degraded sync and needs its own words:
	// this node has no database at all, rather than one that has fallen out of
	// step. The wait is unbounded — a wrong URL or a rejected token loops
	// forever — while the daemon holds the lock throughout, so `hap status`
	// says "running" for a process doing nothing. Past the isolation window it
	// is reported as the stuck install it almost certainly is.
	FleetSyncBootstrapping bool
	// FleetSyncDegraded: the last sync operation failed, at any age. This is
	// the WARNING level and nothing more — a single failed tick is weather,
	// and a banner that fires on one is a banner nobody reads by Friday.
	FleetSyncDegraded bool
	// FleetSyncIsolated: no successful pull OR push for longer than
	// daemonhealth.FleetSyncIsolatedAfter. This is the one that matters, and
	// it ranks with the hard failures: the daemon is running perfectly and
	// every read it serves is a LIE OF OMISSION — the fleet's escalations and
	// agents are not visible here, and this machine's are not reaching them.
	// It looks exactly like a quiet herd, which is why it needs saying out
	// loud rather than leaving to a status line an operator has to think to
	// run.
	FleetSyncIsolated bool
	// FleetSyncFor is how long the current outage has run.
	FleetSyncFor time.Duration
	// FleetSyncError is the last sync error, for the detail lines. The banner
	// deliberately does NOT carry it: what an operator must act on is that
	// this machine is cut off, not that a TLS handshake failed.
	FleetSyncError string
	// FleetSyncDiagLines is the evidence behind it — the descriptor budget at
	// the last failure, and whether an automatic recovery has been spent.
	FleetSyncDiagLines []string
	// OrchestratorFailing: the full-self-prompting orchestrator session could
	// not be started (or adopted, or briefed) and the daemon is retrying.
	// OrchestratorWaiting: it is up, but its brief is held by a claude prompt
	// only the operator should answer. Both are WARNINGS: the herd is served
	// either way, but without them the only trace was the daemon log.
	OrchestratorFailing bool
	OrchestratorWaiting bool
	// OrchestratorError is the failure itself; OrchestratorLine the status
	// page's full rendering (retry timing included).
	OrchestratorError string
	OrchestratorLine  string
	// AgyWorkspaceLines names agy agents working outside the directory they
	// were started in, one advisory line each. A WARNING at most: the herd is
	// served correctly either way, and hap answers the resulting prompts
	// itself once the rule graduates. What it buys the operator is knowing
	// that RELAUNCHING the agent removes those prompts at the source — which
	// neither this page nor `hap agents` says, though the cwd column has
	// carried the raw fact all along.
	AgyWorkspaceLines []string
	// Reason explains a gave-up / auto-disabled latch.
	Reason string
	// StderrLog is the captured daemon stderr path (for hung/crashed post-mortem).
	StderrLog string
}

// AssessDaemonHealth reads the daemon's lock, heartbeat, and crash-loop state.
func (a *App) AssessDaemonHealth() DaemonHealth {
	var h DaemonHealth
	if a.DaemonInfo != nil {
		h.Running, h.PID, h.Version = a.DaemonInfo()
		// Version-staleness needs only the lock record, not any state file, so
		// compute it before the StateDir short-circuit below.
		if h.Running {
			h.VersionStale = h.Version != buildinfo.Version
		}
	}
	if a.StateDir == "" {
		return h
	}
	h.StderrLog = daemonhealth.StderrLogPath(a.StateDir)

	now := time.Now()
	if h.Running {
		// Trust a heartbeat only from THIS lock holder: a hard abort skips the
		// daemon's cleanup, so a dead predecessor's stale record can coexist
		// with a fresh daemon holding the lock — attributing it would
		// false-flag the new daemon during its startup window.
		if rec, ok := daemonhealth.Read(a.StateDir); ok && rec.PID == h.PID {
			h.HeartbeatAge = rec.Age(now)
			h.Hung = rec.Stale(now)
			if rec.Embedder == daemonhealth.EmbedderDegraded {
				h.EmbedderDegraded = true
				h.EmbedderNote = rec.EmbedderNote()
			}
			h.EmbedderDiagLines = rec.EmbedderDiagLines()
			h.BinaryReplaced = rec.BinaryReplaced
			h.FleetSyncLine = rec.FleetSync.Line(now)
			h.FleetSyncDegraded = rec.FleetSync.Degraded()
			h.FleetSyncBootstrapping = rec.FleetSync != nil && !rec.FleetSync.Bootstrapped
			h.FleetSyncIsolated = rec.FleetSync.Isolated(now)
			h.FleetSyncFor = rec.FleetSync.IsolatedFor(now)
			h.FleetSyncDiagLines = rec.FleetSync.DiagLines(now)
			if rec.FleetSync != nil {
				h.FleetSyncError = rec.FleetSync.LastError
			}
			if o := rec.Orchestrator; o != nil {
				h.OrchestratorFailing = o.Failing()
				h.OrchestratorWaiting = o.Waiting
				h.OrchestratorError = o.LastError
				h.OrchestratorLine = o.Line(now)
			}
			for _, m := range rec.AgyWorkspace {
				if line := m.Line(); line != "" {
					h.AgyWorkspaceLines = append(h.AgyWorkspaceLines, line)
				}
			}
		}
	}
	if g, ok := crashguard.Read(a.StateDir); ok {
		if g.EmbeddingOff {
			h.EmbeddingAutoDisabled = true
			h.Reason = g.Reason
		}
		// A give-up only matters while nothing is running (respawns suppressed).
		if g.GaveUp && !h.Running {
			h.GaveUp = true
			h.Reason = g.Reason
		}
		// Down with a recent boot cluster = crash-looping (crashing + being
		// respawned) but not yet given up. Count only boots within the breaker's
		// window so a stale record from an old, since-recovered loop doesn't
		// false-flag; a lone recent boot is ambiguous with a brief clean run, so
		// require a cluster (>=2).
		if !h.Running {
			for _, s := range g.Starts {
				if now.Sub(s) <= crashguard.Window {
					h.RecentRestarts++
				}
			}
			if h.RecentRestarts >= 2 && !h.GaveUp {
				h.CrashLooping = true
			}
		}
	}
	return h
}

// Severity ranks the health for a single-banner/exit-code front-end.
func (h DaemonHealth) Severity() DaemonSeverity {
	switch {
	// BinaryReplaced ranks with the hard failures, not with STALE: a stale
	// daemon still works (it is merely old), whereas one whose binary is gone
	// cannot spawn the MCP server or the embed worker at all — every consult
	// comes back empty, which is indistinguishable from broken automation.
	// FleetSyncIsolated ranks with the hard failures for the same reason
	// BinaryReplaced does: the daemon is alive and beating, and every fleet
	// read it serves is silently incomplete. A herd that looks quiet because
	// the other machines' escalations cannot reach this screen is worse than
	// one that looks broken.
	// A bootstrap that has outlasted the isolation window is an install that is
	// not coming up on its own: the retry is unbounded, the daemon holds the
	// lock while it waits, and NOTHING is being monitored — worse than an
	// isolated node, which at least still answers for its own herd.
	case h.Hung || h.GaveUp || h.CrashLooping || h.BinaryReplaced || h.FleetSyncIsolated ||
		(h.FleetSyncBootstrapping && h.FleetSyncFor >= daemonhealth.FleetSyncIsolatedAfter):
		return DaemonError
	case h.EmbeddingAutoDisabled || h.EmbedderDegraded || h.FleetSyncDegraded || h.OrchestratorFailing ||
		h.OrchestratorWaiting || (h.Running && h.VersionStale):
		return DaemonWarn
	default:
		return DaemonOK
	}
}

// Banner is a one-line summary for an unhealthy daemon, or "" when OK. The most
// severe condition wins so a single line is never ambiguous.
func (h DaemonHealth) Banner() string {
	switch {
	case h.GaveUp:
		return "⚠ DAEMON NOT STARTING — crash-loop breaker gave up: " + h.Reason
	case h.CrashLooping:
		return fmt.Sprintf("⚠ DAEMON DOWN — crash-looping (%d restarts within %s); see %s",
			h.RecentRestarts, crashguard.Window, h.StderrLog)
	case h.Hung:
		return fmt.Sprintf("⚠ DAEMON NOT RESPONDING — no heartbeat for %s; see %s", formatAge(h.HeartbeatAge), h.StderrLog)
	case h.BinaryReplaced:
		return "⚠ DAEMON BINARY REMOVED (upgraded underneath it) — LLM consults cannot run; run: hap daemon --ensure"
	// Ranked above the isolation cases: a node with no database yet is not
	// monitoring at all, and the remedy is different (the URL and the token,
	// not the sync engine). Named separately from the "still starting" case
	// below so a cold start does not read as a broken install.
	case h.FleetSyncBootstrapping && h.FleetSyncFor >= daemonhealth.FleetSyncIsolatedAfter:
		return fmt.Sprintf("⚠ DAEMON NOT MONITORING — still waiting %s for the shared database to bootstrap; "+
			"nothing is being watched. Check database.turso_database_url and the auth token", formatAge(h.FleetSyncFor))
	// Leads with the CONSEQUENCE, not the fault: an operator needs to know
	// that what this screen shows is only half the fleet. The error text is a
	// detail line (FleetSyncDiagLines / the status line), not this.
	case h.FleetSyncIsolated:
		return fmt.Sprintf("⚠ FLEET SYNC ISOLATED for %s — this machine is NOT exchanging rows with the other nodes; "+
			"their escalations and agents are not shown here and this node's are not reaching them", formatAge(h.FleetSyncFor))
	case h.EmbeddingAutoDisabled:
		return "⚠ semantic matching AUTO-DISABLED by crash-loop breaker — " + h.Reason
	case h.EmbedderDegraded:
		return "⚠ embedder degraded — running on BM25 text fallback"
	case h.FleetSyncBootstrapping:
		return "⚠ waiting for the shared database to bootstrap — this node is not monitoring yet"
	case h.FleetSyncDegraded:
		return "⚠ fleet sync failing — this machine may fall out of step with the other nodes"
	case h.OrchestratorFailing:
		return "⚠ orchestrator could not start — " + truncateHealthText(h.OrchestratorError, 160) +
			" (retrying; hap status for details)"
	case h.OrchestratorWaiting:
		return "⚠ orchestrator is waiting on a claude prompt in workspace hap-orchestrator — answer it once and hap sends the brief"
	case h.Running && h.VersionStale:
		return "⚠ daemon is STALE (older binary) — run: hap daemon --ensure"
	default:
		return ""
	}
}

// DaemonStderrTail returns the tail of the daemon's captured stderr log (the
// native-crash reason a hung/crash-looping daemon leaves behind, e.g. the #82
// GGML_ASSERT) and the log's path. The banner names the path; this makes the
// content reachable in-app (#83). It never errors — an empty StateDir, absent
// file, or read failure degrades to empty tail, mirroring AssessDaemonHealth.
func (a *App) DaemonStderrTail() (path, tail string) {
	if a.StateDir == "" {
		return "", ""
	}
	path = daemonhealth.StderrLogPath(a.StateDir)
	f, err := os.Open(path)
	if err != nil {
		return path, ""
	}
	defer f.Close()
	if fi, statErr := f.Stat(); statErr == nil && fi.Size() > maxStderrTailBytes {
		// Seek to the last maxStderrTailBytes; a partial first line is
		// acceptable for a post-mortem tail.
		_, _ = f.Seek(-maxStderrTailBytes, io.SeekEnd)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return path, ""
	}
	return path, strings.TrimRight(string(data), "\n \t")
}

// formatAge renders a heartbeat age compactly ("45s", "2m0s").
func formatAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return d.Round(time.Second).String()
}

// truncateHealthText cuts s to at most n runes, marking the cut with "…", so
// one banner line stays one line whatever herdr put in an error.
func truncateHealthText(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
