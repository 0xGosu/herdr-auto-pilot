package domain

import (
	"strings"
	"testing"
	"time"
)

func TestAgentWaitActiveIsAQuestionAboutNow(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		w    AgentWait
		want bool
	}{
		{"never declared", AgentWait{}, false},
		{"standing", AgentWait{Until: now.Add(time.Minute)}, true},
		{"lapsed", AgentWait{Until: now.Add(-time.Second)}, false},
		// Exactly at the deadline is OVER. The reader is what ends a
		// declaration — nothing writes on expiry — so the boundary has to fall
		// on the side that lets hap start asking again.
		{"exactly at the deadline", AgentWait{Until: now}, false},
	}
	for _, c := range cases {
		if got := c.w.Active(now); got != c.want {
			t.Errorf("%s: Active = %v, want %v", c.name, got, c.want)
		}
	}
	if got := (AgentWait{Until: now.Add(-time.Hour)}).Remaining(now); got != 0 {
		t.Errorf("a lapsed wait has %s remaining, want 0", got)
	}
	if got := (AgentWait{Until: now.Add(90 * time.Second)}).Remaining(now); got != 90*time.Second {
		t.Errorf("Remaining = %s", got)
	}
}

// TestValidateWaitDurationBoundsBothEnds pins the two bounds and the one value
// that is deliberately NOT an error.
func TestValidateWaitDurationBoundsBothEnds(t *testing.T) {
	// Zero is how every surface spells "I finished early"; refusing it would
	// leave an agent no way to end its own declaration.
	if err := ValidateWaitDuration(0); err != nil {
		t.Errorf("a zero duration must clear a wait, not fail: %v", err)
	}
	for _, d := range []time.Duration{-time.Minute, time.Second, MinDeclaredWait - time.Nanosecond,
		MaxDeclaredWait + time.Second} {
		if err := ValidateWaitDuration(d); err == nil {
			t.Errorf("%s was accepted", d)
		}
	}
	for _, d := range []time.Duration{MinDeclaredWait, 20 * time.Minute, MaxDeclaredWait} {
		if err := ValidateWaitDuration(d); err != nil {
			t.Errorf("%s was refused: %v", d, err)
		}
	}
}

func TestNormalizeWaitReasonFoldsAndClips(t *testing.T) {
	if got := NormalizeWaitReason("  cold\nnative   build \n"); got != "cold native build" {
		t.Errorf("got %q", got)
	}
	long := strings.Repeat("x", maxWaitReasonRunes+50)
	if got := NormalizeWaitReason(long); len([]rune(got)) != maxWaitReasonRunes {
		t.Errorf("clipped to %d runes, want %d", len([]rune(got)), maxWaitReasonRunes)
	}
	// Rune-safe: a multi-byte reason must not be cut mid-sequence.
	multi := strings.Repeat("é", maxWaitReasonRunes+10)
	if got := NormalizeWaitReason(multi); len([]rune(got)) != maxWaitReasonRunes {
		t.Errorf("multi-byte clipped to %d runes", len([]rune(got)))
	}
}

func TestShortDuration(t *testing.T) {
	cases := map[time.Duration]string{
		0:                "0s",
		-time.Minute:     "0s",
		45 * time.Second: "45s",
		13*time.Minute + 59*time.Second + 612*time.Millisecond: "14m",
		time.Hour:                 "1h0m",
		time.Hour + 6*time.Minute: "1h6m",
	}
	for d, want := range cases {
		if got := ShortDuration(d); got != want {
			t.Errorf("ShortDuration(%s) = %q, want %q", d, got, want)
		}
	}
}
