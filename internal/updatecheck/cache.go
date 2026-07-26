package updatecheck

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// FileName is the cached check record inside the state dir.
const FileName = "update.check.json"

// TTL is how long a check result (success OR failure) is reused before the
// next fetch. A failure backs off exactly like a success so a host with no
// network path retries at most this often instead of on every tick.
const TTL = 6 * time.Hour

// State is the persisted result of the last release check.
type State struct {
	// CheckedAt is when the last SUCCESSFUL fetch completed.
	CheckedAt time.Time `json:"checked_at"`
	// LatestVersion is the release tag that fetch reported ("v0.5.2").
	LatestVersion string `json:"latest_version"`
	// FailedAt is when the last fetch failed; it backs the retry off without
	// discarding a previously known LatestVersion.
	FailedAt time.Time `json:"failed_at,omitempty"`
}

// Path is the cache file's location in the state dir.
func Path(stateDir string) string { return filepath.Join(stateDir, FileName) }

// Write atomically persists s (temp file + rename, matching daemonhealth.Write
// and config.Save), so a concurrent reader never sees a torn record.
func Write(stateDir string, s State) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(stateDir, ".update-check-*.json")
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

// Read returns the persisted record and whether one was found. A missing file
// yields ok=false with no error; so does a malformed one — a corrupt cache is
// treated as "never checked", never as a failure.
func Read(stateDir string) (State, bool) {
	data, err := os.ReadFile(Path(stateDir))
	if err != nil {
		return State{}, false
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return State{}, false
	}
	return s, true
}

// Due reports whether a fresh fetch should run: never checked, or the last
// attempt (successful or failed) is older than ttl. A record stamped in the
// future — a clock jump — is treated as due rather than trusted forever.
func Due(s State, now time.Time, ttl time.Duration) bool {
	last := s.CheckedAt
	if s.FailedAt.After(last) {
		last = s.FailedAt
	}
	if last.IsZero() {
		return true
	}
	if last.After(now) {
		return true
	}
	return now.Sub(last) >= ttl
}
