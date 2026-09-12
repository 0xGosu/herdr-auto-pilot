package daemonhealth

import (
	"strings"
	"testing"
	"time"
)

// TestIsolationIsMeasuredFromTheLastSUCCESS, not from the last error: a node
// failing every 15 seconds has a one-second-old error and may still have been
// cut off for an hour.
func TestIsolationIsMeasuredFromTheLastSuccess(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	f := &FleetSyncHealth{
		Bootstrapped: true,
		LastPullAt:   now.Add(-30 * time.Minute),
		LastPushAt:   now.Add(-31 * time.Minute),
		LastError:    "SecPolicyCreateSSL error: 0",
		LastErrorAt:  now.Add(-time.Second),
	}
	if got := f.IsolatedFor(now); got != 30*time.Minute {
		t.Errorf("IsolatedFor = %s, want 30m (from the newest SUCCESS, not the newest error)", got)
	}
	if !f.Isolated(now) {
		t.Error("half an hour without a successful sync must read as isolated")
	}
}

// TestANodeThatHasNEVERSyncedIsStillMeasurable is the case that makes
// FirstFailureAt necessary: LastPullAt and LastPushAt are both zero on a daemon
// whose sync has failed since start — exactly the state the incident was
// reported in ("last pull never, last push never") — and without the anchor the
// outage would compute as zero and never raise a banner.
func TestANodeThatHasNeverSyncedIsStillMeasurable(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	f := &FleetSyncHealth{
		Bootstrapped:   true,
		LastError:      "SecPolicyCreateSSL error: 0",
		FirstFailureAt: now.Add(-20 * time.Minute),
	}
	if got := f.IsolatedFor(now); got != 20*time.Minute {
		t.Errorf("IsolatedFor = %s, want 20m anchored on the first failure", got)
	}
	if !f.Isolated(now) {
		t.Fatal("a node that has never synced and has been failing for 20m is isolated")
	}

	// But one that never BOOTSTRAPPED is a different state and must not answer
	// yes here, however long it has been waiting: it has no database at all, so
	// "your peers cannot reach you" is both wrong and points at the wrong fix.
	// The duration is still measurable — the bootstrap banner needs it.
	never := &FleetSyncHealth{
		Bootstrapped:   false,
		LastError:      "turso: bootstrap from the remote failed: 401 Unauthorized",
		FirstFailureAt: now.Add(-25 * time.Minute),
	}
	if !never.Degraded() {
		t.Error("an unbootstrapped node is still degraded")
	}
	if never.Isolated(now) {
		t.Error("a node that never joined the fleet cannot be isolated from it")
	}
	if never.IsolatedFor(now) != 25*time.Minute {
		t.Errorf("IsolatedFor = %s, want the wait to stay measurable for the bootstrap banner",
			never.IsolatedFor(now))
	}
}

// TestAFreshFailureIsNotYetIsolation is the control: without it, code that
// answers "isolated" for any error at all passes every other case here.
func TestAFreshFailureIsNotYetIsolation(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	f := &FleetSyncHealth{
		Bootstrapped:   true,
		LastPullAt:     now.Add(-20 * time.Second),
		LastError:      "temporary blip",
		FirstFailureAt: now.Add(-15 * time.Second),
	}
	if !f.Degraded() {
		t.Error("a failed tick is degraded")
	}
	if f.Isolated(now) {
		t.Error("one failed tick must not read as isolation, or the banner is noise")
	}
}

// TestAHealthySyncIsNeitherDegradedNorIsolated, and a nil record (the local
// engine, or an older daemon) answers the same rather than panicking.
func TestAHealthySyncIsNeitherDegradedNorIsolated(t *testing.T) {
	now := time.Now()
	ok := &FleetSyncHealth{Bootstrapped: true, LastPullAt: now.Add(-time.Second)}
	if ok.Degraded() || ok.Isolated(now) || ok.IsolatedFor(now) != 0 || ok.DiagLines(now) != nil {
		t.Error("a healthy sync must report nothing")
	}
	var nilRec *FleetSyncHealth
	if nilRec.Degraded() || nilRec.Isolated(now) || nilRec.IsolatedFor(now) != 0 ||
		nilRec.DiagLines(now) != nil || nilRec.Line(now) != "" {
		t.Error("a nil record (local engine / older daemon) must be silent, not panic")
	}
}

// TestTheStatusLineNamesTheStateAndItsAge: an operator reading one line must be
// able to tell a blip from a node that has been cut off since breakfast.
func TestTheStatusLineNamesTheStateAndItsAge(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	f := &FleetSyncHealth{
		Bootstrapped:        true,
		LastError:           "SecPolicyCreateSSL error: 0",
		FirstFailureAt:      now.Add(-42 * time.Minute),
		ConsecutiveFailures: 168,
	}
	line := f.Line(now)
	for _, want := range []string{"ISOLATED", "42m0s", "168 consecutive failures", "SecPolicyCreateSSL"} {
		if !strings.Contains(line, want) {
			t.Errorf("status line %q does not carry %q", line, want)
		}
	}
}

// TestDiagLinesCarryTheDescriptorEvidence: the fd probe exists so the NEXT
// occurrence is a one-command diagnosis. A reading that proves exhaustion says
// so and names the remedy; one that merely measured the budget says it did not.
func TestDiagLinesCarryTheDescriptorEvidence(t *testing.T) {
	now := time.Now()
	exhausted := &FleetSyncHealth{
		Bootstrapped: true, LastError: "too many open files", FirstFailureAt: now.Add(-time.Minute),
		FDSoft: 256, FDHard: 4096, FDExhausted: true,
	}
	got := strings.Join(exhausted.DiagLines(now), "\n")
	if !strings.Contains(got, "EXHAUSTED") || !strings.Contains(got, "256") || !strings.Contains(got, "ulimit -n") {
		t.Errorf("an exhausted budget must name itself and its remedy, got %q", got)
	}

	// The discriminating case: a platform that cannot COUNT descriptors (macOS
	// has no /proc/self/fd) must not render its zero as "0 open" — that reads
	// as proof of the opposite of what happened.
	uncounted := &FleetSyncHealth{
		Bootstrapped: true, LastError: "SecPolicyCreateSSL error: 0", FirstFailureAt: now.Add(-time.Minute),
		FDSoft: 256, FDHard: 4096,
	}
	got = strings.Join(uncounted.DiagLines(now), "\n")
	if strings.Contains(got, "0 open") {
		t.Errorf("an uncountable descriptor count was rendered as zero: %q", got)
	}
	if !strings.Contains(got, "unavailable") || !strings.Contains(got, "not exhausted") {
		t.Errorf("a measured-but-not-exhausted budget must say so, got %q", got)
	}
}

// TestDiagLinesSayWhenTheAutomaticRecoveryIsSpent: a node still isolated after
// its own restart is the case that needs a human, and nothing else says so.
func TestDiagLinesSayWhenTheAutomaticRecoveryIsSpent(t *testing.T) {
	now := time.Now()
	f := &FleetSyncHealth{
		Bootstrapped: true, LastError: "SecPolicyCreateSSL error: 0", FirstFailureAt: now.Add(-10 * time.Minute),
		RecoveryLatched: true, RecoveredAt: now.Add(-9 * time.Minute),
	}
	got := strings.Join(f.DiagLines(now), "\n")
	if !strings.Contains(got, "did not help") || !strings.Contains(got, "human") {
		t.Errorf("a spent recovery must say that a human is needed now, got %q", got)
	}
}

// TestADeliberatePauseIsNeverAFailure: the whole reporting contract for
// database.turso_sync_paused, on the hardest input — a record that was failing,
// and isolated, at the moment the operator paused it. The pause freezes
// LastError, the failure count and both timestamps, so every clock-derived
// answer would go on aging; reported as DEGRADED or ISOLATED it teaches an
// operator to ignore the banner that means a real outage.
//
// Degraded is the single choke point (IsolatedFor returns 0 for a sync that is
// not degraded), so asserting all four here is what proves the cascade rather
// than one branch of it.
func TestADeliberatePauseIsNeverAFailure(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	paused := &FleetSyncHealth{
		Engine: "turso", Bootstrapped: true, Paused: true,
		LastPullAt:          now.Add(-3 * time.Hour),
		LastPushAt:          now.Add(-3 * time.Hour),
		LastError:           "SecPolicyCreateSSL error: 0",
		FirstFailureAt:      now.Add(-3 * time.Hour),
		ConsecutiveFailures: 400,
		PendingOps:          17,
		FDSoft:              256, FDHard: 4096, FDExhausted: true,
	}
	if paused.Degraded() {
		t.Error("a paused sync must not be degraded — nothing is being attempted")
	}
	if paused.Isolated(now) || paused.IsolatedFor(now) != 0 {
		t.Errorf("a paused sync read as isolated for %s", paused.IsolatedFor(now))
	}
	if paused.DiagLines(now) != nil {
		t.Errorf("a paused sync produced failure evidence: %q", paused.DiagLines(now))
	}
	line := paused.Line(now)
	if !strings.Contains(line, "PAUSED") || !strings.Contains(line, "turso_sync_paused") {
		t.Errorf("status line %q must name the pause and the key that ends it", line)
	}
	// The unpushed count is how an operator judges when to lift the pause.
	if !strings.Contains(line, "17 unpushed") {
		t.Errorf("status line %q does not carry the queued-write count", line)
	}
	for _, unwanted := range []string{"DEGRADED", "ISOLATED", "BOOTSTRAP", "SecPolicyCreateSSL"} {
		if strings.Contains(line, unwanted) {
			t.Errorf("status line %q reports a paused sync as %q", line, unwanted)
		}
	}

	// The control: the identical record with the pause off is the failure it
	// always was. Without this, a Degraded() hardwired to false passes above.
	running := *paused
	running.Paused = false
	if !running.Degraded() || !running.Isolated(now) {
		t.Fatal("the same record with the pause OFF must still be degraded and isolated")
	}
	if !strings.Contains(running.Line(now), "ISOLATED") {
		t.Errorf("unpaused status line = %q, want ISOLATED", running.Line(now))
	}
}
