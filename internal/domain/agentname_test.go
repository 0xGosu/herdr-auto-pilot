package domain

import (
	"regexp"
	"testing"
)

func TestGenerateAgentNameDeterministic(t *testing.T) {
	none := func(string) bool { return false }
	a := GenerateAgentName("w6:p1", none)
	b := GenerateAgentName("w6:p1", none)
	if a != b {
		t.Errorf("same agent id should generate the same name: %q vs %q", a, b)
	}
	if !regexp.MustCompile(`^[a-z]+-[a-z]+$`).MatchString(a) {
		t.Errorf("name %q should be adjective-animal", a)
	}
	if c := GenerateAgentName("w11:p3", none); c == a {
		t.Errorf("different agents should (almost always) differ: both %q", c)
	}
}

func TestGenerateAgentNameAvoidsTaken(t *testing.T) {
	none := func(string) bool { return false }
	first := GenerateAgentName("w1:p1", none)
	second := GenerateAgentName("w1:p1", func(n string) bool { return n == first })
	if second == first {
		t.Error("taken name must not be reused")
	}
}

func TestGenerateAgentNameExhaustionFallsBack(t *testing.T) {
	// All combos taken: numbered fallback still yields a unique name.
	name := GenerateAgentName("w9:p9", func(n string) bool {
		return !regexp.MustCompile(`-\d+$`).MatchString(n)
	})
	if !regexp.MustCompile(`^[a-z]+-[a-z]+-\d+$`).MatchString(name) {
		t.Errorf("expected numbered fallback, got %q", name)
	}
}
