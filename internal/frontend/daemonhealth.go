package frontend

import (
	"fmt"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/buildinfo"
	"github.com/0xGosu/herdr-auto-pilot/internal/crashguard"
	"github.com/0xGosu/herdr-auto-pilot/internal/daemonhealth"
)

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
	case h.Hung || h.GaveUp || h.CrashLooping:
		return DaemonError
	case h.EmbeddingAutoDisabled || h.EmbedderDegraded || (h.Running && h.VersionStale):
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
	case h.EmbeddingAutoDisabled:
		return "⚠ semantic matching AUTO-DISABLED by crash-loop breaker — " + h.Reason
	case h.EmbedderDegraded:
		return "⚠ embedder degraded — running on BM25 text fallback"
	case h.Running && h.VersionStale:
		return "⚠ daemon is STALE (older binary) — run: hap daemon --ensure"
	default:
		return ""
	}
}

// formatAge renders a heartbeat age compactly ("45s", "2m0s").
func formatAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return d.Round(time.Second).String()
}
