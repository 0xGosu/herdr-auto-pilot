package updatecheck

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := State{
		CheckedAt:     time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC),
		LatestVersion: "v0.5.2",
	}
	if err := Write(dir, want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, ok := Read(dir)
	if !ok {
		t.Fatal("Read reported no record after Write")
	}
	if got.LatestVersion != want.LatestVersion || !got.CheckedAt.Equal(want.CheckedAt) {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
}

// TestWriteLeavesNoTempFiles guards the temp+rename write: a leaked temp file
// in the state dir would accumulate on every check.
func TestWriteLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, State{LatestVersion: "v1.0.0"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != FileName {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("state dir = %v, want just %q", names, FileName)
	}
}

// TestReadMissingOrMalformed pins "no signal, never an error": a corrupt cache
// must read as "never checked" rather than break the TUI.
func TestReadMissingOrMalformed(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		if _, ok := Read(t.TempDir()); ok {
			t.Error("Read reported a record for an empty state dir")
		}
	})
	t.Run("malformed", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, FileName), []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, ok := Read(dir); ok {
			t.Error("Read accepted a malformed record")
		}
	})
}

func TestDue(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		st   State
		want bool
	}{
		{"never checked", State{}, true},
		{"just checked", State{CheckedAt: now.Add(-time.Minute)}, false},
		{"one tick short of the ttl", State{CheckedAt: now.Add(-TTL + time.Second)}, false},
		{"exactly at the ttl", State{CheckedAt: now.Add(-TTL)}, true},
		{"long past the ttl", State{CheckedAt: now.Add(-48 * time.Hour)}, true},
		// A failure backs off like a success, so an offline host does not
		// retry on every tick.
		{"recent failure backs off", State{FailedAt: now.Add(-time.Minute)}, false},
		{"stale failure retries", State{FailedAt: now.Add(-TTL)}, true},
		{"failure after an old success still backs off",
			State{CheckedAt: now.Add(-48 * time.Hour), FailedAt: now.Add(-time.Minute)}, false},
		// A record stamped in the future (clock jump) must not pin the check
		// off forever.
		{"future timestamp is due", State{CheckedAt: now.Add(time.Hour)}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Due(tc.st, now, TTL); got != tc.want {
				t.Errorf("Due(%+v) = %v, want %v", tc.st, got, tc.want)
			}
		})
	}
}
