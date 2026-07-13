package crashguard

import (
	"os"
	"testing"
	"time"
)

var base = time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)

// boot runs one Evaluate at base+offset and returns the decision, threading
// state through a scenario.
func boot(st State, offsetSec int, digest string) (State, Decision) {
	return Evaluate(st, base.Add(time.Duration(offsetSec)*time.Second), digest)
}

func TestNormalBootsBelowThresholdDoNothing(t *testing.T) {
	var st State
	var d Decision
	st, d = boot(st, 0, "cfg")
	if d != (Decision{}) {
		t.Fatalf("first boot decision = %+v, want zero", d)
	}
	st, d = boot(st, 10, "cfg")
	if d != (Decision{}) {
		t.Fatalf("second boot decision = %+v, want zero", d)
	}
	if st.EmbeddingOff || st.GaveUp {
		t.Errorf("two boots must not latch anything: %+v", st)
	}
}

func TestThirdBootWithinWindowAutoDisablesEmbedding(t *testing.T) {
	var st State
	st, _ = boot(st, 0, "cfg")
	st, _ = boot(st, 10, "cfg")
	st, d := boot(st, 20, "cfg")
	if !d.DisableEmbedding || d.GiveUp {
		t.Fatalf("third boot within window should auto-disable embedding, got %+v", d)
	}
	if !st.EmbeddingOff {
		t.Error("EmbeddingOff must latch")
	}
	if d.Reason == "" {
		t.Error("a mitigation decision should carry a reason")
	}
}

func TestBootsSpreadBeyondWindowNeverTrip(t *testing.T) {
	var st State
	var d Decision
	// One boot every 60s: never 3 within the 90s window.
	for i := 0; i < 6; i++ {
		st, d = boot(st, i*60, "cfg")
		if d != (Decision{}) {
			t.Fatalf("boot %d spread beyond window tripped: %+v", i, d)
		}
	}
}

func TestStillLoopingWithEmbeddingOffGivesUp(t *testing.T) {
	var st State
	st, _ = boot(st, 0, "cfg")
	st, _ = boot(st, 10, "cfg")
	st, _ = boot(st, 20, "cfg") // auto-disables embedding, resets history to [20]

	// One more boot after the auto-disable is not yet a fresh cluster.
	st, d := boot(st, 30, "cfg")
	if d.GiveUp {
		t.Fatalf("one boot after auto-disable must not give up yet, got %+v", d)
	}
	// A fresh 3-boot cluster WITH the embedder already off = still looping → give up.
	st, d = boot(st, 40, "cfg")
	if !d.GiveUp {
		t.Fatalf("a fresh cluster with the embedder off should give up, got %+v", d)
	}
	if !st.GaveUp {
		t.Error("GaveUp must latch")
	}
}

func TestCleanRestartAfterAutoDisableDoesNotGiveUp(t *testing.T) {
	// Regression: the auto-disable boot must reset the boot history so a single
	// legitimate restart within the window does NOT escalate to a full stop.
	var st State
	st, _ = boot(st, 0, "cfg")
	st, _ = boot(st, 10, "cfg")
	st, _ = boot(st, 20, "cfg") // auto-disable, history reset to [20]
	_, d := boot(st, 25, "cfg") // one clean restart soon after
	if d.GiveUp {
		t.Fatalf("a lone restart after auto-disable must not give up, got %+v", d)
	}
	if !d.DisableEmbedding {
		t.Errorf("the embedder-off latch should still hold, got %+v", d)
	}
}

func TestLatchesPersistAcrossBootsUntilConfigChange(t *testing.T) {
	var st State
	st, _ = boot(st, 0, "cfg")
	st, _ = boot(st, 10, "cfg")
	st, _ = boot(st, 20, "cfg") // EmbeddingOff latched

	// Much later boot, loop history pruned, but the latch persists.
	st, d := boot(st, 500, "cfg")
	if !d.DisableEmbedding {
		t.Fatalf("EmbeddingOff latch must persist after the loop clears, got %+v", d)
	}
	if len(st.Starts) != 1 {
		t.Errorf("old starts should have pruned, got %d", len(st.Starts))
	}

	// A config change clears the latch → clean attempt.
	st, d = boot(st, 510, "cfg-v2")
	if d != (Decision{}) {
		t.Fatalf("config change should clear the latch, got %+v", d)
	}
	if st.EmbeddingOff || st.GaveUp {
		t.Errorf("config change must clear latches: %+v", st)
	}
}

func TestGiveUpPersistsUntilConfigChange(t *testing.T) {
	st := State{GaveUp: true, ConfigDigest: "cfg", Reason: "boom"}
	_, d := boot(st, 0, "cfg")
	if !d.GiveUp {
		t.Fatalf("give-up must persist with unchanged config, got %+v", d)
	}
	st2, d := boot(st, 0, "cfg-v2")
	if d.GiveUp {
		t.Fatalf("config change must lift give-up, got %+v", d)
	}
	if st2.GaveUp {
		t.Error("GaveUp must clear on config change")
	}
}

func TestSurvivedClearsStartsKeepsLatch(t *testing.T) {
	st := State{
		Starts:       []time.Time{base, base.Add(time.Second)},
		EmbeddingOff: true, ConfigDigest: "cfg", Reason: "disabled",
	}
	got, changed := st.Survived()
	if !changed {
		t.Fatal("Survived should report a change when starts are present")
	}
	if len(got.Starts) != 0 {
		t.Errorf("Survived must clear starts, got %d", len(got.Starts))
	}
	if !got.EmbeddingOff {
		t.Error("Survived must keep the EmbeddingOff latch")
	}
	// Idempotent when already empty.
	if _, changed := got.Survived(); changed {
		t.Error("Survived on empty starts should report no change")
	}
}

func TestSpawnBlocked(t *testing.T) {
	// Not gave-up → never blocked.
	if blocked, _, _ := SpawnBlocked(State{}, "cfg"); blocked {
		t.Error("no give-up must not block spawn")
	}
	// Gave-up, same digest → blocked.
	gu := State{GaveUp: true, ConfigDigest: "cfg", Reason: "boom"}
	blocked, _, reason := SpawnBlocked(gu, "cfg")
	if !blocked || reason == "" {
		t.Errorf("give-up with unchanged config must block with a reason, got blocked=%v reason=%q", blocked, reason)
	}
	// Gave-up, changed digest → not blocked, cleared state returned.
	blocked, cleared, _ := SpawnBlocked(gu, "cfg-v2")
	if blocked {
		t.Error("config change must lift the spawn block")
	}
	if cleared.GaveUp || cleared.ConfigDigest != "cfg-v2" {
		t.Errorf("cleared state should reset with the new digest, got %+v", cleared)
	}
}

func TestEmbeddingSuppressed(t *testing.T) {
	// No latch → never suppressed.
	if supp, _, _ := EmbeddingSuppressed(State{}, "cfg"); supp {
		t.Error("no latch must not suppress")
	}
	// Latched, same digest → suppressed.
	latched := State{EmbeddingOff: true, ConfigDigest: "cfg"}
	if supp, _, changed := EmbeddingSuppressed(latched, "cfg"); !supp || changed {
		t.Errorf("latched + same digest: supp=%v changed=%v, want supp=true changed=false", supp, changed)
	}
	// Latched, changed digest → not suppressed, cleared state returned for live re-enable.
	supp, cleared, changed := EmbeddingSuppressed(latched, "cfg-v2")
	if supp || !changed {
		t.Errorf("latched + changed digest: supp=%v changed=%v, want supp=false changed=true", supp, changed)
	}
	if cleared.EmbeddingOff || cleared.ConfigDigest != "cfg-v2" {
		t.Errorf("cleared state should reset with the new digest, got %+v", cleared)
	}
}

func TestReadWriteRemove(t *testing.T) {
	dir := t.TempDir()
	if _, ok := Read(dir); ok {
		t.Error("missing file should read ok=false")
	}
	want := State{Starts: []time.Time{base}, EmbeddingOff: true, ConfigDigest: "cfg", Reason: "x"}
	if err := Write(dir, want); err != nil {
		t.Fatal(err)
	}
	got, ok := Read(dir)
	if !ok || !got.EmbeddingOff || got.ConfigDigest != "cfg" {
		t.Errorf("round trip mismatch: %+v ok=%v", got, ok)
	}
	// Malformed → ok=false, never fatal.
	os.WriteFile(Path(dir), []byte("{bad"), 0o600)
	if _, ok := Read(dir); ok {
		t.Error("malformed file should read ok=false")
	}
	if err := Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := Remove(dir); err != nil {
		t.Errorf("removing an absent file must not error: %v", err)
	}
}

func TestWriteIsAtomicNoTempLeftover(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, State{}); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != FileName {
		t.Errorf("expected only %s in dir, got %v", FileName, entries)
	}
}
