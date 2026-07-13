package daemonhealth

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestWriteReadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	want := Health{
		PID:         4242,
		Version:     "v9.9.9",
		StartedAt:   now.Add(-time.Minute),
		HeartbeatAt: now,
		Embedder:    EmbedderReady,
	}
	if err := Write(dir, want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, ok := Read(dir)
	if !ok {
		t.Fatal("Read: ok=false after Write")
	}
	if got.PID != want.PID || got.Version != want.Version || got.Embedder != want.Embedder {
		t.Errorf("round trip mismatch: got %+v want %+v", got, want)
	}
	if !got.HeartbeatAt.Equal(want.HeartbeatAt) {
		t.Errorf("HeartbeatAt: got %v want %v", got.HeartbeatAt, want.HeartbeatAt)
	}
}

func TestReadMissingFile(t *testing.T) {
	if _, ok := Read(t.TempDir()); ok {
		t.Error("Read of missing file: ok=true, want false")
	}
}

func TestReadMalformedFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(Path(dir), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := Read(dir); ok {
		t.Error("Read of malformed file: ok=true, want false (corrupt heartbeat = no signal)")
	}
}

func TestWriteIsAtomic(t *testing.T) {
	// After a successful Write the state dir holds exactly the health file —
	// no leftover temp file from the temp+rename dance.
	dir := t.TempDir()
	if err := Write(dir, Health{PID: 1}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != FileName {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("state dir = %v, want just [%s]", names, FileName)
	}
}

func TestStale(t *testing.T) {
	now := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		heartbeat time.Time
		wantStale bool
	}{
		{"fresh", now.Add(-5 * time.Second), false},
		{"just under threshold", now.Add(-(StaleAfter - time.Second)), false},
		{"just over threshold", now.Add(-(StaleAfter + time.Second)), true},
		{"never written (zero)", time.Time{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := Health{HeartbeatAt: tc.heartbeat}
			if got := h.Stale(now); got != tc.wantStale {
				t.Errorf("Stale = %v, want %v", got, tc.wantStale)
			}
		})
	}
}

func TestRemove(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, Health{PID: 1}); err != nil {
		t.Fatal(err)
	}
	if err := Remove(dir); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(Path(dir)); !os.IsNotExist(err) {
		t.Error("file still present after Remove")
	}
	// Removing an absent file is not an error.
	if err := Remove(dir); err != nil {
		t.Errorf("Remove of missing file: %v", err)
	}
}

func TestEmbedderNoteDegradedActionable(t *testing.T) {
	note := Health{Embedder: EmbedderDegraded}.EmbedderNote()
	if note == string(EmbedderDegraded) {
		t.Error("degraded note should include a remediation hint")
	}
	if got := (Health{Embedder: EmbedderReady}).EmbedderNote(); got != string(EmbedderReady) {
		t.Errorf("ready note = %q, want plain %q", got, EmbedderReady)
	}
}

func TestOpenStderrLogAppendsThenRotates(t *testing.T) {
	dir := t.TempDir()

	// First open creates the file at the expected path.
	f := OpenStderrLog(dir)
	if f == nil {
		t.Fatal("OpenStderrLog returned nil on a fresh dir")
	}
	if _, err := f.WriteString("first crash\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := os.Stat(StderrLogPath(dir)); err != nil {
		t.Fatalf("stderr log not created: %v", err)
	}

	// A second open below the cap appends (keeps prior content).
	f = OpenStderrLog(dir)
	if _, err := f.WriteString("second crash\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	data, _ := os.ReadFile(StderrLogPath(dir))
	if !strings.Contains(string(data), "first crash") || !strings.Contains(string(data), "second crash") {
		t.Errorf("below cap should append, got: %q", data)
	}

	// Grow past the cap, then open again → prior content rotates to .old and
	// the live log starts fresh (the crash-loop disk guard).
	if err := os.WriteFile(StderrLogPath(dir), make([]byte, StderrLogCap+1), 0o600); err != nil {
		t.Fatal(err)
	}
	f = OpenStderrLog(dir)
	if f == nil {
		t.Fatal("OpenStderrLog returned nil after growth")
	}
	f.Close()
	if _, err := os.Stat(StderrLogPath(dir) + ".old"); err != nil {
		t.Errorf("oversized log should rotate to .old: %v", err)
	}
	if fi, err := os.Stat(StderrLogPath(dir)); err != nil || fi.Size() != 0 {
		t.Errorf("post-rotation live log should be fresh/empty, size err=%v", err)
	}
}

func TestAgeFlooredAtZero(t *testing.T) {
	now := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	// A heartbeat slightly in the future (clock skew) floors to zero, not negative.
	if got := (Health{HeartbeatAt: now.Add(time.Second)}).Age(now); got != 0 {
		t.Errorf("Age with future heartbeat = %v, want 0", got)
	}
	if got := (Health{HeartbeatAt: now.Add(-3 * time.Second)}).Age(now); got != 3*time.Second {
		t.Errorf("Age = %v, want 3s", got)
	}
}
