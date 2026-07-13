package frontend

import (
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/buildinfo"
	"github.com/0xGosu/herdr-auto-pilot/internal/crashguard"
	"github.com/0xGosu/herdr-auto-pilot/internal/daemonhealth"
)

func appWithDaemon(t *testing.T, running bool, pid int, ver string) *App {
	t.Helper()
	dir := t.TempDir()
	return &App{
		StateDir:   dir,
		DaemonInfo: func() (bool, int, string) { return running, pid, ver },
	}
}

func TestAssessHealthyRunning(t *testing.T) {
	app := appWithDaemon(t, true, 100, buildinfo.Version)
	if err := daemonhealth.Write(app.StateDir, daemonhealth.Health{
		PID: 100, Version: buildinfo.Version, HeartbeatAt: time.Now(),
		Embedder: daemonhealth.EmbedderReady,
	}); err != nil {
		t.Fatal(err)
	}
	h := app.AssessDaemonHealth()
	if !h.Running || h.Hung || h.EmbedderDegraded {
		t.Errorf("healthy daemon misclassified: %+v", h)
	}
	if h.Severity() != DaemonOK {
		t.Errorf("severity = %v, want OK", h.Severity())
	}
	if h.Banner() != "" {
		t.Errorf("healthy daemon should have no banner, got %q", h.Banner())
	}
}

func TestAssessNotRunningClean(t *testing.T) {
	app := appWithDaemon(t, false, 0, "")
	h := app.AssessDaemonHealth()
	if h.Severity() != DaemonOK || h.Banner() != "" {
		t.Errorf("a cleanly stopped daemon is not an alert: %+v banner=%q", h, h.Banner())
	}
}

func TestAssessHung(t *testing.T) {
	app := appWithDaemon(t, true, 100, buildinfo.Version)
	daemonhealth.Write(app.StateDir, daemonhealth.Health{
		PID: 100, Version: buildinfo.Version,
		HeartbeatAt: time.Now().Add(-2 * daemonhealth.StaleAfter),
		Embedder:    daemonhealth.EmbedderReady,
	})
	h := app.AssessDaemonHealth()
	if !h.Hung || h.Severity() != DaemonError {
		t.Errorf("stale heartbeat should be hung/error: %+v", h)
	}
	if !strings.Contains(h.Banner(), "NOT RESPONDING") {
		t.Errorf("banner = %q, want NOT RESPONDING", h.Banner())
	}
}

func TestAssessIgnoresStaleRecordFromOtherPid(t *testing.T) {
	app := appWithDaemon(t, true, 100, buildinfo.Version)
	// Stale record from a dead predecessor (pid 999), not this lock holder (100).
	daemonhealth.Write(app.StateDir, daemonhealth.Health{
		PID: 999, HeartbeatAt: time.Now().Add(-2 * daemonhealth.StaleAfter),
	})
	h := app.AssessDaemonHealth()
	if h.Hung {
		t.Errorf("a different pid's stale record must not mark this daemon hung: %+v", h)
	}
}

func TestAssessEmbedderDegraded(t *testing.T) {
	app := appWithDaemon(t, true, 100, buildinfo.Version)
	daemonhealth.Write(app.StateDir, daemonhealth.Health{
		PID: 100, Version: buildinfo.Version, HeartbeatAt: time.Now(),
		Embedder: daemonhealth.EmbedderDegraded,
	})
	h := app.AssessDaemonHealth()
	if !h.EmbedderDegraded || h.Severity() != DaemonWarn {
		t.Errorf("degraded embedder should be warn: %+v", h)
	}
	if !strings.Contains(h.Banner(), "degraded") {
		t.Errorf("banner = %q, want degraded", h.Banner())
	}
	// The note carries the real remediation command for the CLI to render.
	if !strings.Contains(h.EmbedderNote, "hap config set embedding.disabled") {
		t.Errorf("EmbedderNote must carry the actionable command, got %q", h.EmbedderNote)
	}
}

func TestAssessEmbeddingAutoDisabled(t *testing.T) {
	app := appWithDaemon(t, true, 100, buildinfo.Version)
	daemonhealth.Write(app.StateDir, daemonhealth.Health{
		PID: 100, Version: buildinfo.Version, HeartbeatAt: time.Now(),
		Embedder: daemonhealth.EmbedderDisabled,
	})
	crashguard.Write(app.StateDir, crashguard.State{
		EmbeddingOff: true, ConfigDigest: "cfg", Reason: "auto-disabled after crash-loop",
	})
	h := app.AssessDaemonHealth()
	if !h.EmbeddingAutoDisabled || h.Severity() != DaemonWarn {
		t.Errorf("auto-disabled embedding should be warn: %+v", h)
	}
	if !strings.Contains(h.Banner(), "AUTO-DISABLED") {
		t.Errorf("banner = %q, want AUTO-DISABLED", h.Banner())
	}
}

func TestAssessGaveUp(t *testing.T) {
	app := appWithDaemon(t, false, 0, "") // breaker suppressed the daemon
	crashguard.Write(app.StateDir, crashguard.State{
		GaveUp: true, ConfigDigest: "cfg", Reason: "looping even with embedder off",
	})
	h := app.AssessDaemonHealth()
	if !h.GaveUp || h.Severity() != DaemonError {
		t.Errorf("give-up should be error: %+v", h)
	}
	if !strings.Contains(h.Banner(), "NOT STARTING") {
		t.Errorf("banner = %q, want NOT STARTING", h.Banner())
	}
}

func TestAssessVersionStale(t *testing.T) {
	app := appWithDaemon(t, true, 100, "v0.0.1-old")
	daemonhealth.Write(app.StateDir, daemonhealth.Health{
		PID: 100, Version: "v0.0.1-old", HeartbeatAt: time.Now(),
		Embedder: daemonhealth.EmbedderReady,
	})
	h := app.AssessDaemonHealth()
	if !h.VersionStale || h.Severity() != DaemonWarn {
		t.Errorf("older binary should be warn/stale: %+v", h)
	}
	if !strings.Contains(h.Banner(), "STALE") {
		t.Errorf("banner = %q, want STALE", h.Banner())
	}
}

func TestAssessVersionStaleWithoutStateDir(t *testing.T) {
	// Version-staleness must be detectable from DaemonInfo alone (no state dir),
	// since it only compares the lock's recorded version to this binary.
	app := &App{DaemonInfo: func() (bool, int, string) { return true, 100, "v0.0.1-old" }}
	h := app.AssessDaemonHealth()
	if !h.VersionStale {
		t.Errorf("version-stale must be detected without a StateDir: %+v", h)
	}
	if h.Severity() != DaemonWarn {
		t.Errorf("severity = %v, want warn", h.Severity())
	}
}

// Give-up outranks auto-disable in the single banner.
func TestBannerSeverityPrecedence(t *testing.T) {
	h := DaemonHealth{GaveUp: true, EmbeddingAutoDisabled: true, Reason: "boom"}
	if !strings.Contains(h.Banner(), "NOT STARTING") {
		t.Errorf("give-up must win the banner over auto-disable, got %q", h.Banner())
	}
}
