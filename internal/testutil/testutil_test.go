package testutil

import (
	"testing"
	"time"
)

func TestScaleFromPrefersTheEnvironmentOverTheHeuristic(t *testing.T) {
	// The env knob is what CI sets, and it must win on a runner with plenty of
	// cores — the whole point is that core count cannot see contention.
	if got := scaleFrom("4", 16); got != 4 {
		t.Errorf("scaleFrom(\"4\", 16) = %v, want 4", got)
	}
	if got := scaleFrom("2.5", 2); got != 2.5 {
		t.Errorf("a fractional scale must be honoured, got %v", got)
	}
}

func TestScaleFromIgnoresAnUnusableEnvironmentValue(t *testing.T) {
	// A typo in CI's environment must not fail every suite, and a value below 1
	// must never SHORTEN a wait — both fall back to the heuristic, which on this
	// many cores is 1.
	for _, env := range []string{"", "abc", "0", "0.5", "-3"} {
		if got := scaleFrom(env, 16); got != 1 {
			t.Errorf("scaleFrom(%q, 16) = %v, want the 1x fallback", env, got)
		}
	}
}

func TestScaleFromStretchesForASmallRunner(t *testing.T) {
	for _, c := range []struct {
		procs int
		want  float64
	}{{1, 4}, {2, 4}, {3, 4}, {4, 2}, {7, 2}, {8, 1}, {32, 1}} {
		if got := scaleFrom("", c.procs); got != c.want {
			t.Errorf("scaleFrom(\"\", %d) = %v, want %v", c.procs, got, c.want)
		}
	}
}

func TestScaleOnlyEverLengthens(t *testing.T) {
	// Scale reads the process-wide cached factor, which is floored at 1 by
	// scaleFrom, so the guarantee callers rely on is that a deadline never
	// comes back shorter than the one they asked for.
	base := 3 * time.Second
	if got := Scale(base); got < base {
		t.Errorf("Scale(%s) = %s, must never shorten a deadline", base, got)
	}
	if s := TimeoutScale(); s < 1 {
		t.Errorf("TimeoutScale() = %v, must never be below 1", s)
	}
}
