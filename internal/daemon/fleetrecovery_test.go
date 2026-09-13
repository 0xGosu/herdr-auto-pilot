package daemon

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// wedgedDaemon builds a daemon whose fleet sync has been failing for the given
// outage with the given error, wired to a RestartSelf that records its calls.
func wedgedDaemon(t *testing.T, syncErr string, failures int, outage time.Duration) (*Daemon, *[]string) {
	t.Helper()
	var calls []string
	d := &Daemon{opt: Options{
		StateDir:    t.TempDir(),
		Clock:       ports.SystemClock{},
		FleetSync:   &fakeFleetSync{},
		RestartSelf: func(reason string) error { calls = append(calls, reason); return nil },
	}}
	now := time.Now()
	d.fleet.lastError = syncErr
	d.fleet.lastErrorAt = now
	d.fleet.consecutiveFailures = failures
	d.fleet.firstFailureAt = now.Add(-outage)
	return d, &calls
}

// TestAWedgedSyncEngineRestartsTheDaemon: the whole point. A process-local
// fault that has isolated this node past both bounds hands the herd to a fresh
// daemon and leaves a marker for it, so the successor knows it was born from a
// recovery.
func TestAWedgedSyncEngineRestartsTheDaemon(t *testing.T) {
	d, calls := wedgedDaemon(t, "tls: failed to verify certificate: SecPolicyCreateSSL error: 0",
		fleetRecoveryMinFailures, fleetRecoveryMinOutage+time.Minute)
	if !d.checkFleetSyncWedged() {
		t.Fatal("a wedged sync engine past both bounds did not restart the daemon")
	}
	if len(*calls) != 1 {
		t.Fatalf("RestartSelf calls = %d, want 1", len(*calls))
	}
	// Latched, but NOT handed off: the `hap daemon --restart` just spawned is
	// what stops this process, so until its signal lands this daemon is still
	// the herd's only monitor.
	if !d.fleetRecoveryOrdered.Load() {
		t.Error("ordering a restart did not latch; the next heartbeat would order another")
	}
	if d.handedOff.Load() {
		t.Error("a sync recovery must not claim a binary handover — Run would exit and leave the herd unmonitored")
	}
	// And the very next beat orders nothing more.
	if d.checkFleetSyncWedged() || len(*calls) != 1 {
		t.Fatalf("a second restart was ordered on the next heartbeat: %d calls", len(*calls))
	}
	m, ok := readFleetRecoveryMarker(d.opt.StateDir)
	if !ok {
		t.Fatal("no recovery marker was left; the successor would restart again on the same fault")
	}
	if m.ConsecutiveFailures != fleetRecoveryMinFailures || m.Error == "" {
		t.Errorf("marker did not record what it was recovering from: %+v", m)
	}
}

// TestARemoteOutageNeverRestartsTheDaemon is the control that makes the test
// above mean something: the same shape of outage, a fault out on the network,
// and nothing is restarted. Without it, code that restarts on ANY isolation
// passes the whole file.
func TestARemoteOutageNeverRestartsTheDaemon(t *testing.T) {
	for _, remote := range []string{
		`Post "https://db.turso.io/v2/pipeline": context deadline exceeded`,
		`Post "https://db.turso.io/v2/pipeline": dial tcp: lookup db.turso.io: no such host`,
		`sync engine error: 401 Unauthorized`,
	} {
		d, calls := wedgedDaemon(t, remote, fleetRecoveryMinFailures*3, fleetRecoveryMinOutage*4)
		if d.checkFleetSyncWedged() {
			t.Errorf("restarted the daemon over a remote fault: %s", remote)
		}
		if len(*calls) != 0 {
			t.Errorf("RestartSelf called for a remote fault: %s", remote)
		}
		if _, ok := readFleetRecoveryMarker(d.opt.StateDir); ok {
			t.Errorf("left a recovery marker for a fault it declined to recover from: %s", remote)
		}
	}
}

// TestAnOutageMustClearBOTHBounds: a count without the elapsed time, and an
// elapsed time without the count, each leave the daemon alone. They are
// separate gates because either alone fires on ordinary weather — a fast pull
// interval racks up failures in seconds, and one old failure is not an outage.
func TestAnOutageMustClearBOTHBounds(t *testing.T) {
	local := "tls: failed to verify certificate: SecPolicyCreateSSL error: 0"
	t.Run("enough failures, too short", func(t *testing.T) {
		d, calls := wedgedDaemon(t, local, fleetRecoveryMinFailures*10, fleetRecoveryMinOutage-time.Second)
		if d.checkFleetSyncWedged() || len(*calls) != 0 {
			t.Fatal("restarted before the outage was long enough")
		}
	})
	t.Run("long enough, too few failures", func(t *testing.T) {
		d, calls := wedgedDaemon(t, local, fleetRecoveryMinFailures-1, fleetRecoveryMinOutage*10)
		if d.checkFleetSyncWedged() || len(*calls) != 0 {
			t.Fatal("restarted on fewer failures than the bound")
		}
	})
	t.Run("no failure at all", func(t *testing.T) {
		d, calls := wedgedDaemon(t, "", 0, 0)
		if d.checkFleetSyncWedged() || len(*calls) != 0 {
			t.Fatal("restarted a healthy node")
		}
	})
}

// TestADaemonBornFromARecoveryDoesNotOrderAnother: the latch. Without it a
// fault a fresh process cannot clear becomes a restart loop that abandons the
// herd's in-flight work every cooldown — strictly worse than the isolation.
func TestADaemonBornFromARecoveryDoesNotOrderAnother(t *testing.T) {
	local := "too many open files"
	d, calls := wedgedDaemon(t, local, fleetRecoveryMinFailures, fleetRecoveryMinOutage*2)
	if err := writeFleetRecoveryMarker(d.opt.StateDir, fleetRecoveryMarker{
		RestartedAt: time.Now().Add(-time.Minute), Error: local,
	}); err != nil {
		t.Fatal(err)
	}
	d.adoptFleetRecoveryMarker()
	if !d.fleet.recoveryLatched {
		t.Fatal("adopting a fresh marker did not latch the recovery")
	}
	if d.checkFleetSyncWedged() || len(*calls) != 0 {
		t.Fatal("a daemon born from a recovery ordered another one")
	}
}

// TestARecoveryMarkerPastItsCooldownDoesNotLatch: an hour-old failure and a
// fresh one need not share a cause, so the daemon is allowed one more attempt
// — and the stale marker is dropped rather than left to latch a later daemon.
func TestARecoveryMarkerPastItsCooldownDoesNotLatch(t *testing.T) {
	local := "too many open files"
	d, calls := wedgedDaemon(t, local, fleetRecoveryMinFailures, fleetRecoveryMinOutage*2)
	if err := writeFleetRecoveryMarker(d.opt.StateDir, fleetRecoveryMarker{
		RestartedAt: time.Now().Add(-fleetRecoveryCooldown - time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	d.adoptFleetRecoveryMarker()
	if d.fleet.recoveryLatched {
		t.Fatal("a marker past its cooldown still latched")
	}
	if !d.checkFleetSyncWedged() || len(*calls) != 1 {
		t.Fatal("a stale marker blocked a recovery it should not have")
	}
}

// TestASuccessfulSyncReleasesTheRecoveryLatch: the latch is released by
// EVIDENCE — one pull or push that worked — not by a timer.
func TestASuccessfulSyncReleasesTheRecoveryLatch(t *testing.T) {
	local := "too many open files"
	d, calls := wedgedDaemon(t, local, fleetRecoveryMinFailures, fleetRecoveryMinOutage*2)
	if err := writeFleetRecoveryMarker(d.opt.StateDir, fleetRecoveryMarker{
		RestartedAt: time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	d.adoptFleetRecoveryMarker()
	d.fleetRecoveryDeclined.Store(true)

	d.noteFleetSuccess()

	if d.fleet.recoveryLatched || d.fleet.consecutiveFailures != 0 || d.fleet.lastError != "" ||
		!d.fleet.firstFailureAt.IsZero() {
		t.Fatalf("a success did not end the outage: failures=%d err=%q first=%v latched=%v",
			d.fleet.consecutiveFailures, d.fleet.lastError, d.fleet.firstFailureAt, d.fleet.recoveryLatched)
	}
	if d.fleetRecoveryDeclined.Load() {
		t.Error("the once-per-outage explanation was not re-armed for the next outage")
	}
	if _, ok := readFleetRecoveryMarker(d.opt.StateDir); ok {
		t.Error("a success did not clear the recovery marker")
	}
	// And the next outage may recover again.
	now := time.Now()
	d.fleet.lastError = local
	d.fleet.consecutiveFailures = fleetRecoveryMinFailures
	d.fleet.firstFailureAt = now.Add(-fleetRecoveryMinOutage * 2)
	d.lastFleetRecovery.Store(0)
	d.fleetRecoveryOrdered.Store(false)
	if !d.checkFleetSyncWedged() || len(*calls) != 1 {
		t.Fatal("a released latch did not allow the next recovery")
	}
}

// TestAFailedRecoverySpawnLeavesNoMarker: the successor never started, so this
// daemon stays up — and the marker must go, or the successor of some LATER
// restart would be latched by a recovery that never happened.
func TestAFailedRecoverySpawnLeavesNoMarker(t *testing.T) {
	d, _ := wedgedDaemon(t, "too many open files", fleetRecoveryMinFailures, fleetRecoveryMinOutage*2)
	d.opt.RestartSelf = func(string) error { return errors.New("fork refused") }
	if d.checkFleetSyncWedged() {
		t.Fatal("reported an ordered restart after the spawn failed")
	}
	if d.fleetRecoveryOrdered.Load() || d.handedOff.Load() {
		t.Error("a failed spawn latched anyway, so the retry would never happen")
	}
	if _, ok := readFleetRecoveryMarker(d.opt.StateDir); ok {
		t.Error("a failed spawn left a marker that would latch a future successor")
	}
}

// TestAnUnwritableMarkerRefusesTheRestart: without a marker the successor
// cannot know it was born from a recovery, so an unfixable fault would become a
// restart loop. Refusing is the safe direction — the node stays isolated and
// says so.
func TestAnUnwritableMarkerRefusesTheRestart(t *testing.T) {
	d, calls := wedgedDaemon(t, "too many open files", fleetRecoveryMinFailures, fleetRecoveryMinOutage*2)
	// A directory where the marker's name must be a file.
	if err := os.Mkdir(fleetRecoveryPath(d.opt.StateDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if d.checkFleetSyncWedged() || len(*calls) != 0 {
		t.Fatal("restarted without being able to record the marker")
	}
}

// TestNoSeamLeavesTheNodeIsolated: RestartSelf is nil in every front end and
// under the local engine, and a nil seam must never panic or pretend.
func TestNoSeamLeavesTheNodeIsolated(t *testing.T) {
	d, _ := wedgedDaemon(t, "too many open files", fleetRecoveryMinFailures, fleetRecoveryMinOutage*2)
	d.opt.RestartSelf = nil
	if d.checkFleetSyncWedged() {
		t.Fatal("ordered a restart with no seam wired")
	}
	d.opt.RestartSelf = func(string) error { return nil }
	d.opt.FleetSync = nil
	if d.checkFleetSyncWedged() {
		t.Fatal("ordered a restart under the local engine")
	}
}

// TestARecoveryIsNotRetriedEveryHeartbeat: a spawn that keeps being refused
// must not storm. One attempt per interval.
func TestARecoveryIsNotRetriedEveryHeartbeat(t *testing.T) {
	d, calls := wedgedDaemon(t, "too many open files", fleetRecoveryMinFailures, fleetRecoveryMinOutage*2)
	d.opt.RestartSelf = func(reason string) error {
		*calls = append(*calls, reason)
		return errors.New("fork refused")
	}
	for range 5 {
		d.checkFleetSyncWedged()
	}
	if len(*calls) != 1 {
		t.Fatalf("attempted the recovery %d times in a row; it must be throttled", len(*calls))
	}
}

// TestFailuresCountBothDirections: a node that pulls fine and cannot push is
// just as isolated as one that can do neither, so the counter spans both — and
// a success in EITHER direction ends the outage.
func TestFailuresCountBothDirections(t *testing.T) {
	var s fleetSyncState
	now := time.Now()
	s.fail(now, errors.New("push failed"))
	s.fail(now.Add(time.Second), errors.New("pull failed"))
	if s.consecutiveFailures != 2 {
		t.Fatalf("consecutiveFailures = %d, want 2 across both directions", s.consecutiveFailures)
	}
	if !s.firstFailureAt.Equal(now) {
		t.Error("firstFailureAt must anchor the FIRST failure of the outage, not the latest")
	}
	if !s.noteSuccess() {
		t.Error("noteSuccess must report that it ended an outage, so the marker is cleared once")
	}
	if s.consecutiveFailures != 0 || s.lastError != "" || !s.firstFailureAt.IsZero() {
		t.Fatalf("a success did not reset the outage: failures=%d err=%q first=%v",
			s.consecutiveFailures, s.lastError, s.firstFailureAt)
	}
	if s.noteSuccess() {
		t.Error("a success on a healthy node must not report an outage ending, or the marker is unlinked every tick")
	}
}

// TestAPausedSyncNeverRestartsTheDaemon: a pause freezes lastError, the failure
// count and both timestamps while the outage's own clock keeps running, so a
// node that happened to be failing when the operator paused it satisfies every
// bound a few minutes later. Restarting there abandons the herd's in-flight
// captures and consults, every fleetRecoveryRetryInterval, to fix something
// nobody asked it to do — elapsed time is only evidence of an outage while
// something is still trying.
//
// Driven directly, as the daemon's guards must be: an end-to-end test cannot
// reach this decision.
func TestAPausedSyncNeverRestartsTheDaemon(t *testing.T) {
	d, calls := wedgedDaemon(t, "tls: failed to verify certificate: SecPolicyCreateSSL error: 0",
		fleetRecoveryMinFailures*3, fleetRecoveryMinOutage*4)
	// The same daemon restarts itself with the pause off — this is the control
	// that makes the assertion below mean something.
	if !d.checkFleetSyncWedged() {
		t.Fatal("this fault must be one a restart clears, or the paused case proves nothing")
	}
	d.fleetRecoveryOrdered.Store(false)
	clearFleetRecoveryMarker(d.opt.StateDir)
	*calls = nil

	d.mu.Lock()
	d.cfg.Database.TursoSyncPaused = true
	d.mu.Unlock()
	if d.checkFleetSyncWedged() {
		t.Error("restarted the daemon over a sync the operator deliberately paused")
	}
	if len(*calls) != 0 {
		t.Errorf("RestartSelf calls = %d while paused, want 0", len(*calls))
	}
	if _, ok := readFleetRecoveryMarker(d.opt.StateDir); ok {
		t.Error("a recovery marker was written for a paused node")
	}
}
