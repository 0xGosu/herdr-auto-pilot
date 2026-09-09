package frontend

import (
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/buildinfo"
	"github.com/0xGosu/herdr-auto-pilot/internal/daemonhealth"
)

// syncHealth writes a heartbeat whose fleet sync is in the given state and
// returns the assessment a front end would make of it.
func syncHealth(t *testing.T, fs *daemonhealth.FleetSyncHealth) DaemonHealth {
	t.Helper()
	app := appWithDaemon(t, true, 4242, buildinfo.Version)
	if err := daemonhealth.Write(app.StateDir, daemonhealth.Health{
		PID:         4242,
		Version:     buildinfo.Version,
		StartedAt:   time.Now().Add(-time.Hour),
		HeartbeatAt: time.Now(),
		Embedder:    daemonhealth.EmbedderReady,
		FleetSync:   fs,
	}); err != nil {
		t.Fatal(err)
	}
	return app.AssessDaemonHealth()
}

// TestAnIsolatedNodeIsADaemonError: the daemon is running perfectly and every
// fleet read it serves is silently incomplete — the other machines' escalations
// and agents are not on this screen, and this node's are not reaching them.
// That looks exactly like a quiet herd, which is why it ranks with the hard
// failures rather than with the warnings.
func TestAnIsolatedNodeIsADaemonError(t *testing.T) {
	now := time.Now()
	h := syncHealth(t, &daemonhealth.FleetSyncHealth{
		Engine: "turso", Bootstrapped: true,
		LastError:           "tls: failed to verify certificate: SecPolicyCreateSSL error: 0",
		LastErrorAt:         now,
		FirstFailureAt:      now.Add(-40 * time.Minute),
		ConsecutiveFailures: 160,
		FDSoft:              256, FDHard: 4096,
	})
	if !h.FleetSyncIsolated || !h.FleetSyncDegraded {
		t.Fatalf("a 40-minute outage did not read as isolation: %+v", h)
	}
	if h.Severity() != DaemonError {
		t.Errorf("Severity = %v, want DaemonError for an isolated node", h.Severity())
	}
	banner := h.Banner()
	// The banner leads with the CONSEQUENCE. An operator needs to know that
	// what this screen shows is half the fleet, not that a handshake failed.
	for _, want := range []string{"ISOLATED", "not shown here", "not reaching them"} {
		if !strings.Contains(banner, want) {
			t.Errorf("banner %q does not say what the isolation MEANS (%q)", banner, want)
		}
	}
	if strings.Contains(banner, "SecPolicyCreateSSL") {
		t.Errorf("the raw sync error belongs in the detail lines, not the banner: %q", banner)
	}
	if h.FleetSyncError == "" || len(h.FleetSyncDiagLines) == 0 {
		t.Error("the error and its evidence must still be reachable as detail")
	}
}

// TestAFreshSyncFailureIsOnlyAWarning is the control. Without it, code that
// escalates any sync error at all passes the test above — and an operator
// trained to ignore a banner that fires on every blip will ignore the one that
// matters.
func TestAFreshSyncFailureIsOnlyAWarning(t *testing.T) {
	now := time.Now()
	h := syncHealth(t, &daemonhealth.FleetSyncHealth{
		Engine: "turso", Bootstrapped: true,
		LastError: "temporary blip", LastErrorAt: now,
		LastPullAt:          now.Add(-20 * time.Second),
		FirstFailureAt:      now.Add(-15 * time.Second),
		ConsecutiveFailures: 1,
	})
	if !h.FleetSyncDegraded {
		t.Fatal("a failed tick must still be reported")
	}
	if h.FleetSyncIsolated {
		t.Fatal("one failed tick is not isolation")
	}
	if h.Severity() != DaemonWarn {
		t.Errorf("Severity = %v, want DaemonWarn for a fresh failure", h.Severity())
	}
	if !strings.Contains(h.Banner(), "fleet sync failing") {
		t.Errorf("banner %q does not report the degradation", h.Banner())
	}
}

// TestAHealthySyncSaysNothing: the local engine writes no FleetSync record at
// all, and a healthy turso node must be just as quiet — a banner on a working
// system is the fastest way to make every banner invisible.
func TestAHealthySyncSaysNothing(t *testing.T) {
	for _, fs := range []*daemonhealth.FleetSyncHealth{
		nil,
		{Engine: "turso", Bootstrapped: true, LastPullAt: time.Now().Add(-time.Second)},
	} {
		h := syncHealth(t, fs)
		if h.FleetSyncDegraded || h.FleetSyncIsolated || h.Banner() != "" || h.Severity() != DaemonOK {
			t.Errorf("a healthy node reported something: banner=%q severity=%v", h.Banner(), h.Severity())
		}
	}
}

// TestAStuckBootstrapIsNamedSeparately: a node waiting on its FIRST database
// is not a node that has fallen out of step — it has no database at all, the
// retry in openTurso is unbounded, and the daemon holds the lock while it waits,
// so `hap status` reports "running" for a process monitoring nothing. Past the
// isolation window that is an install that is not coming up, and the remedy is
// the URL and the token rather than anything about the sync engine.
func TestAStuckBootstrapIsNamedSeparately(t *testing.T) {
	now := time.Now()
	h := syncHealth(t, &daemonhealth.FleetSyncHealth{
		Engine: "turso", Bootstrapped: false,
		LastError:      "turso: bootstrap from the remote failed: 401 Unauthorized",
		LastErrorAt:    now,
		FirstFailureAt: now.Add(-25 * time.Minute),
	})
	if !h.FleetSyncBootstrapping {
		t.Fatal("an unbootstrapped node was not reported as one")
	}
	// NOT isolated: isolation means this node was part of a fleet and has been
	// cut off, and every reader that asks only that question — `hap status`
	// asks with a bare `if`, not an ordered switch — would otherwise tell the
	// operator their peers' escalations are not reaching a machine that has no
	// database to receive them in.
	if h.FleetSyncIsolated {
		t.Error("a node that never bootstrapped was reported as isolated")
	}
	if h.Severity() != DaemonError {
		t.Errorf("Severity = %v, want DaemonError for a bootstrap stuck 25 minutes", h.Severity())
	}
	banner := h.Banner()
	if !strings.Contains(banner, "NOT MONITORING") || !strings.Contains(banner, "turso_database_url") {
		t.Errorf("banner %q does not name the state or its remedy", banner)
	}
	// And it must NOT be described as falling out of step — that is the other
	// state, with a different fix.
	if strings.Contains(banner, "fall out of step") {
		t.Errorf("a node with no database was described as a drifting one: %q", banner)
	}
}

// TestAColdBootstrapIsOnlyAWarning is the control: the first seconds of a
// perfectly normal turso start look identical to a stuck one apart from their
// age, so escalating on the state alone would put an error banner on every
// cold start.
func TestAColdBootstrapIsOnlyAWarning(t *testing.T) {
	now := time.Now()
	h := syncHealth(t, &daemonhealth.FleetSyncHealth{
		Engine: "turso", Bootstrapped: false,
		LastError: "turso: bootstrap from the remote failed: dial tcp: i/o timeout", LastErrorAt: now,
		FirstFailureAt: now.Add(-8 * time.Second),
	})
	if !h.FleetSyncBootstrapping {
		t.Fatal("a bootstrapping node was not reported as one")
	}
	if h.Severity() != DaemonWarn {
		t.Errorf("Severity = %v, want DaemonWarn for a cold start", h.Severity())
	}
	if !strings.Contains(h.Banner(), "not monitoring yet") {
		t.Errorf("banner %q does not report the wait", h.Banner())
	}
}
