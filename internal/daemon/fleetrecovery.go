package daemon

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// The fleet-sync recovery marker is how a daemon born from an automatic
// restart learns that it WAS one, and so refuses to order another until it has
// proved the restart worked.
//
// Without it the recovery is a loop rather than a repair: a fault a fresh
// process cannot clear (a permanently broken system trust store, say) would
// restart the daemon every cooldown forever, abandoning the herd's in-flight
// captures and consults each time — strictly worse than the isolation it is
// trying to fix, and far harder to diagnose, because every `hap status` would
// be reading a daemon that is seconds old.
//
// So the latch is released by EVIDENCE, not by time: one successful pull or
// push deletes the marker. The cooldown below is only the ceiling on how long
// a marker keeps latching when nothing ever succeeds — past it the daemon is
// allowed one more attempt, on the theory that an hour-old failure and a fresh
// one may not have the same cause.
const fleetRecoveryCooldown = time.Hour

// fleetRecoveryFile is the marker's basename inside the state dir.
const fleetRecoveryFile = "fleet-sync-recovery.json"

// fleetRecoveryMarker records the restart this daemon's predecessor ordered.
type fleetRecoveryMarker struct {
	RestartedAt time.Time `json:"restarted_at"`
	// Error is the sync failure that prompted it, kept so the successor can
	// say what it was recovering FROM when the recovery does not take.
	Error string `json:"error,omitempty"`
	// ConsecutiveFailures and IsolatedFor record how bad it had got, so the
	// log line the successor writes is worth reading.
	ConsecutiveFailures int    `json:"consecutive_failures,omitempty"`
	IsolatedFor         string `json:"isolated_for,omitempty"`
}

func fleetRecoveryPath(stateDir string) string {
	return filepath.Join(stateDir, fleetRecoveryFile)
}

// readFleetRecoveryMarker returns the marker a predecessor left, if any. A
// missing, unreadable or unparseable file is "no marker": this gates a
// RECOVERY, and failing closed here would mean a corrupted byte permanently
// disabling the one mechanism that fixes a wedged node.
func readFleetRecoveryMarker(stateDir string) (fleetRecoveryMarker, bool) {
	if stateDir == "" {
		return fleetRecoveryMarker{}, false
	}
	data, err := os.ReadFile(fleetRecoveryPath(stateDir))
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Debug("fleet sync: could not read the recovery marker", "error", err)
		}
		return fleetRecoveryMarker{}, false
	}
	var m fleetRecoveryMarker
	if err := json.Unmarshal(data, &m); err != nil || m.RestartedAt.IsZero() {
		return fleetRecoveryMarker{}, false
	}
	return m, true
}

// writeFleetRecoveryMarker records that a restart is being ordered. It is
// written BEFORE the successor is spawned, never after: the successor may be
// running and reading it before this process gets another scheduling slice,
// and a marker that lands late is a marker the successor did not see.
func writeFleetRecoveryMarker(stateDir string, m fleetRecoveryMarker) error {
	if stateDir == "" {
		return errors.New("no state dir")
	}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp := fleetRecoveryPath(stateDir) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, fleetRecoveryPath(stateDir)); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// clearFleetRecoveryMarker releases the latch. Called on the first successful
// sync of any kind, which is the only evidence that the restart worked.
func clearFleetRecoveryMarker(stateDir string) {
	if stateDir == "" {
		return
	}
	if err := os.Remove(fleetRecoveryPath(stateDir)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Debug("fleet sync: could not clear the recovery marker", "error", err)
	}
}
