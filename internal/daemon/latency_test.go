package daemon

import (
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// TestDecisionLatencyP95At25Agents verifies SC-1 / NFR-001 / NFR-002: on the
// rules-only decision path the daemon reaches a decision within p95 ≤ 1s of
// an agent-status transition while 25 agents are active.
func TestDecisionLatencyP95At25Agents(t *testing.T) {
	if testing.Short() {
		t.Skip("latency benchmark skipped in -short mode")
	}
	// Raise the runaway ceilings: the benchmark intentionally sends many
	// consecutive auto-prompts, which the default guard would (correctly)
	// stop at 5.
	h := newHarness(t, "[limits]\nmax_consecutive_auto_prompts = 1000\nmax_auto_prompts_per_minute = 1000\n")
	h.herdr.setPane(approvalPane)
	h.seedAutonomous(approvalPane, domain.SituationApproval, "1")

	const agents = 25
	const rounds = 8 // 200 transitions total

	var latencies []time.Duration
	sent := 0
	for round := 0; round < rounds; round++ {
		for a := 0; a < agents; a++ {
			agentID := fmt.Sprintf("bench-agent-%d", a)
			start := time.Now()
			h.push(agentID, "blocked")
			sent++
			target := sent
			waitFor(t, 5*time.Second, func() bool {
				return len(h.herdr.sentInputs()) >= target
			})
			latencies = append(latencies, time.Since(start))
		}
	}

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	p95 := latencies[len(latencies)*95/100]
	t.Logf("decisions=%d p50=%s p95=%s max=%s",
		len(latencies), latencies[len(latencies)/2], p95, latencies[len(latencies)-1])
	if p95 > time.Second {
		t.Errorf("p95 decision latency %s exceeds the 1s budget (SC-1)", p95)
	}
}
