package fdprobe

import (
	"os"
	"runtime"
	"testing"
)

// TestAHealthyProcessIsNotExhausted: the probe's one load-bearing answer, on a
// process that plainly has descriptors to spare. It must also never claim a
// count it could not take — a zero Open with OpenKnown false is "unavailable",
// and a reader that rendered it as 0 would report the opposite of the truth.
func TestAHealthyProcessIsNotExhausted(t *testing.T) {
	p := Read()
	if p.Exhausted {
		t.Fatal("a test process with descriptors to spare reported exhaustion")
	}
	if p.Open != 0 && !p.OpenKnown {
		t.Error("a nonzero Open must be marked known")
	}
	if runtime.GOOS == "linux" {
		if !p.OpenKnown || p.Open <= 0 {
			t.Errorf("linux has /proc/self/fd, so the count must be available: %+v", p)
		}
		if p.Soft == 0 {
			t.Error("RLIMIT_NOFILE must be readable on linux")
		}
	}
}

// TestTheProbeLeaksNoDescriptor: it runs on every sync failure, which under a
// wedged engine is every fifteen seconds for hours. A probe that leaked the
// handle it opens would itself become the exhaustion it exists to detect.
func TestTheProbeLeaksNoDescriptor(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs /proc/self/fd to count")
	}
	before := Read()
	for range 50 {
		Read()
	}
	after := Read()
	if after.Open > before.Open+2 {
		t.Fatalf("open descriptors grew from %d to %d across 50 probes", before.Open, after.Open)
	}
}

// TestTheCountDiscountsItsOwnHandle: the directory read holds a descriptor
// while it lists them, and reporting a count that is always one high would
// quietly shift every reading the operator compares.
func TestTheCountDiscountsItsOwnHandle(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs /proc/self/fd to count")
	}
	before := Read()
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	after := Read()
	if after.Open != before.Open+1 {
		t.Errorf("holding one more descriptor moved the count from %d to %d, want exactly +1",
			before.Open, after.Open)
	}
}
