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

// TestEmbedderNoteTimeoutBoundPointsAtTimeouts is the diagnostics contract that
// keeps a merely-slow model usable: when every failure was a stall-guard
// expiry, the remedy shown must be raising the budgets — NOT
// `embedding.disabled`, which would throw semantic matching away for good.
func TestEmbedderNoteTimeoutBoundPointsAtTimeouts(t *testing.T) {
	h := Health{
		Embedder: EmbedderDegraded,
		EmbedderDiag: &EmbedderDiag{
			Failures: 3, Timeouts: 3, MaxFailures: 3,
			EmbedTimeoutMs: 2000, WarmTimeoutMs: 30000,
			TimeoutBound: true,
		},
	}
	note := h.EmbedderNote()
	if !strings.Contains(note, "embed_timeout_ms") || !strings.Contains(note, "warm_timeout_ms") {
		t.Errorf("note = %q, want it to name the timeout keys to raise", note)
	}
	if strings.Contains(note, "embedding.disabled") {
		t.Errorf("note = %q, must not advise disabling embeddings for a pure timeout degrade", note)
	}

	// A degrade with non-timeout failures keeps the original wording.
	h.EmbedderDiag.TimeoutBound = false
	if got := h.EmbedderNote(); !strings.Contains(got, "embedding.disabled") {
		t.Errorf("non-timeout degrade note = %q, want the original remediation hint", got)
	}
}

// TestEmbedderDiagLines pins the evidence block `hap status` prints, including
// the "no diagnostics at all" case an older daemon's heartbeat produces.
func TestEmbedderDiagLines(t *testing.T) {
	if lines := (Health{Embedder: EmbedderDegraded}).EmbedderDiagLines(); lines != nil {
		t.Errorf("lines without diagnostics = %v, want none", lines)
	}
	// Nothing has failed: stay silent. The budgets are always populated (they
	// are the resolved defaults), so an unconditional line would print on every
	// healthy status forever.
	if lines := (Health{EmbedderDiag: &EmbedderDiag{EmbedTimeoutMs: 2000, WarmTimeoutMs: 30000}}).EmbedderDiagLines(); lines != nil {
		t.Errorf("lines = %v, want none when nothing has failed", lines)
	}

	lines := Health{EmbedderDiag: &EmbedderDiag{
		Failures: 5, Timeouts: 4, MaxFailures: 3,
		EmbedTimeoutMs: 2000, WarmTimeoutMs: 30000,
		LastError: "embed call exceeded 2s stall guard",
	}}.EmbedderDiagLines()
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"failures: 5 (4 timeouts)", "latch at 3", "budgets: embed 2000ms", "last error: embed call exceeded 2s stall guard"} {
		if !strings.Contains(joined, want) {
			t.Errorf("lines = %v, want a line containing %q", lines, want)
		}
	}
}

// TestHealthRoundTripsDiagnostics guards the wire format: the diagnostics must
// survive the heartbeat file, and a record written without them must read back
// as nil (older daemons) rather than as zeroed counters that look real.
func TestHealthRoundTripsDiagnostics(t *testing.T) {
	dir := t.TempDir()
	in := Health{PID: 42, Embedder: EmbedderDegraded, EmbedderDiag: &EmbedderDiag{
		Failures: 3, Timeouts: 3, MaxFailures: 3, EmbedTimeoutMs: 2000, TimeoutBound: true,
	}}
	if err := Write(dir, in); err != nil {
		t.Fatal(err)
	}
	got, ok := Read(dir)
	if !ok {
		t.Fatal("Read after Write: not found")
	}
	if got.EmbedderDiag == nil {
		t.Fatal("EmbedderDiag lost in the round trip")
	}
	if *got.EmbedderDiag != *in.EmbedderDiag {
		t.Errorf("EmbedderDiag = %+v, want %+v", *got.EmbedderDiag, *in.EmbedderDiag)
	}

	if err := Write(dir, Health{PID: 42, Embedder: EmbedderReady}); err != nil {
		t.Fatal(err)
	}
	if got, _ := Read(dir); got.EmbedderDiag != nil {
		t.Errorf("EmbedderDiag = %+v, want nil when never reported", *got.EmbedderDiag)
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
