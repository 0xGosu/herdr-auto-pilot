package domain

import (
	"bufio"
	"os"
	"strings"
	"testing"
	"time"
)

func newSeedAllowlist(t *testing.T) *Allowlist {
	t.Helper()
	a, errs := NewAllowlist(true, nil, nil)
	if len(errs) > 0 {
		t.Fatalf("seed allowlist failed to compile: %v", errs)
	}
	return a
}

// TestSeedAllowlistCatchesCorpus is the CI regression gate (NFR-005a):
// 100% of the irreversible-op corpus must be matched by seed patterns.
// A corpus miss fails the build.
func TestSeedAllowlistCatchesCorpus(t *testing.T) {
	a := newSeedAllowlist(t)

	f, err := os.Open("testdata/irreversible_corpus.txt")
	if err != nil {
		t.Fatalf("open corpus: %v", err)
	}
	defer f.Close()

	var total, missed int
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		total++
		if _, ok := a.Match(line); !ok {
			missed++
			t.Errorf("corpus entry NOT matched by seed allowlist: %q", line)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	if total < 40 {
		t.Fatalf("corpus suspiciously small (%d entries); maintenance regression?", total)
	}
	t.Logf("allowlist corpus: %d/%d matched", total-missed, total)
}

func TestAllowlistDoesNotMatchBenignPrompts(t *testing.T) {
	a := newSeedAllowlist(t)
	benign := []string{
		"Do you want to run the unit test suite now?",
		"Allow reading the file src/main.go?",
		"Run go build ./... to check compilation?",
		"Should I add a new test for the parser?",
		"Apply the suggested refactor to the config loader?",
		"Run git status and show the diff?",
		"Commit the staged changes with message 'fix: handle nil input'?",
	}
	for _, p := range benign {
		if pat, ok := a.Match(p); ok {
			t.Errorf("benign prompt matched allowlist pattern %q: %q", pat, p)
		}
	}
}

func TestOperatorPatternsExtendSeed(t *testing.T) {
	a, errs := NewAllowlist(true, []string{`(?i)restart\s+the\s+payment\s+service`}, nil)
	if len(errs) > 0 {
		t.Fatalf("compile: %v", errs)
	}
	if _, ok := a.Match("Please restart the payment service now"); !ok {
		t.Error("operator-added pattern should match")
	}
}

func TestInvalidOperatorPatternReported(t *testing.T) {
	_, errs := NewAllowlist(true, []string{`([unclosed`}, nil)
	if len(errs) == 0 {
		t.Error("invalid pattern must be reported")
	}
}

func TestSuspectedIrreversibleHeuristic(t *testing.T) {
	a := newSeedAllowlist(t)
	suspicious := []string{
		"This will permanently erase the workspace metadata. Continue?",
		"Overwrite the existing changes and discard local work?",
		"This action cannot be undone. Proceed?",
	}
	for _, p := range suspicious {
		if !a.SuspectedIrreversible(p) {
			t.Errorf("expected suspected-irreversible for %q", p)
		}
	}
	if a.SuspectedIrreversible("Run the formatter on the changed files?") {
		t.Error("benign prompt should not trip the heuristic")
	}
}

func TestRateGuardFunctions(t *testing.T) {
	now := time.Now()
	lim := RateLimits{MaxConsecutive: 5, MaxPerMinute: 10}

	r := AgentRate{}
	for i := 0; i < 5; i++ {
		ok, _ := CheckRate(r, now, lim)
		if !ok {
			t.Fatalf("prompt %d should be allowed", i+1)
		}
		r = RegisterAutoPrompt(r, now)
	}
	if ok, reason := CheckRate(r, now, lim); ok || reason != ReasonRateLimited {
		t.Error("6th consecutive prompt must be blocked")
	}

	// Human interaction resets the consecutive counter.
	r = RegisterHumanInteraction(r)
	if ok, _ := CheckRate(r, now, lim); !ok {
		t.Error("automation should resume after human interaction")
	}

	// Per-minute window.
	r = AgentRate{WindowStart: now.Add(-10 * time.Second), CountInWindow: 10}
	if ok, _ := CheckRate(r, now, lim); ok {
		t.Error("11th prompt within the minute must be blocked")
	}
	// Window expiry allows again (consecutive still under ceiling).
	r = AgentRate{WindowStart: now.Add(-2 * time.Minute), CountInWindow: 10}
	if ok, _ := CheckRate(r, now, lim); !ok {
		t.Error("expired window should allow prompting again")
	}

	// Paused agents stay blocked until human interaction.
	r = PauseAgent(AgentRate{})
	if ok, _ := CheckRate(r, now, lim); ok {
		t.Error("paused agent must stay blocked")
	}
}
