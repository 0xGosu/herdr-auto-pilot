package daemon

// Tests for the first-interaction command variants (llm.command_start /
// llm.rewrite_command_start): the daemon flags the FIRST consult and FIRST
// rewrite per agent with req.First and marks every later one false. The flag
// is keyed by agent and is NOT reset on "detected" (a subscriber reconnect
// must not re-fire the kickoff prompt); a genuinely new agent arrives with a
// new id and primes naturally.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

func TestConsultFirstFlagPerAgent(t *testing.T) {
	// Default confidence threshold (999) means every consult escalates, which
	// leaves nothing sent and no rate/learning side effects — a clean way to
	// drive repeated consults for one agent.
	h := newHarness(t, "[llm]\ncommand = [\"fake\"]\ntimeout_seconds = 5\n")
	h.llm.configured = true
	var mu sync.Mutex
	type call struct {
		agent string
		first bool
	}
	var calls []call
	h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		mu.Lock()
		calls = append(calls, call{req.AgentID, req.First})
		mu.Unlock()
		return nil, errors.New("escalate") // no submission → the daemon escalates
	}
	got := func() []call {
		mu.Lock()
		defer mu.Unlock()
		return append([]call(nil), calls...)
	}

	// Three distinct situation types → three distinct signatures for the same
	// agent, so each is a fresh consult (no same-signature escalation dedup).
	errorPane := "ERROR: build failed with exit status 2\nRetry, skip, or abort?\n"
	choicePane := "Which framework?\n❯ 1. React\n  2. Vue\n  3. Svelte\n"

	consult := func(agent, pane string, wantLen int) {
		t.Helper()
		h.herdr.setPane(pane)
		h.push(agent, "blocked")
		waitFor(t, 5*time.Second, func() bool { return len(got()) == wantLen })
	}

	consult("agent-cf", approvalPane, 1) // first consult for this agent
	consult("agent-cf", errorPane, 2)    // later consult → not first

	// A subscriber reconnect replays the pane as "detected"; it must NOT
	// re-prime command_start for an agent already mid-session.
	h.push("agent-cf", "detected")
	time.Sleep(50 * time.Millisecond) // let the transition drain
	consult("agent-cf", choicePane, 3)

	// A genuinely new agent (new pane id) primes naturally — no reset needed.
	consult("agent-cf2", approvalPane, 4)

	want := []call{
		{"agent-cf", true},
		{"agent-cf", false},
		{"agent-cf", false}, // after "detected": still not first
		{"agent-cf2", true}, // new agent: first
	}
	seq := got()
	if len(seq) != len(want) {
		t.Fatalf("consult sequence = %v, want %v", seq, want)
	}
	for i := range want {
		if seq[i] != want[i] {
			t.Errorf("consult #%d = %+v, want %+v (full: %v)", i+1, seq[i], want[i], seq)
		}
	}
}

func TestRewriteFirstFlagPerAgent(t *testing.T) {
	// A benign rewritten reply keeps the send flowing; high limits keep the
	// runaway guard out of the way across the repeated auto-sends.
	cfg := "[limits]\nmax_consecutive_auto_prompts = 100\nmax_auto_prompts_per_minute = 100\n"
	h, fr := newHarnessRewriter(t, cfg,
		func(ctx context.Context, req domain.RewriteRequest) (string, error) {
			return "yes please", nil
		})

	// Distinct free-text approvals → distinct signatures for the same agent;
	// each resolves to a literal "y" send that goes through the rewriter.
	drive := func(agent, pane string, wantSends int) {
		t.Helper()
		h.herdr.setPane(pane)
		h.seedAutonomous(pane, domain.SituationApproval, "y")
		h.push(agent, "blocked")
		waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == wantSends })
	}

	drive("agent-rwstart", "Do you want to continue? (y/n)\n", 1)
	drive("agent-rwstart", "Proceed with the build? (y/n)\n", 2)

	// A reconnect ("detected") must NOT re-prime rewrite_command_start.
	h.push("agent-rwstart", "detected")
	time.Sleep(50 * time.Millisecond)
	drive("agent-rwstart", "Save your changes? (y/n)\n", 3)

	// A new agent primes naturally.
	drive("agent-rwstart2", "Ship it? (y/n)\n", 4)

	calls := fr.rewriteCalls()
	if len(calls) != 4 {
		t.Fatalf("expected 4 rewrites, got %d: %+v", len(calls), calls)
	}
	want := []bool{true, false, false, true}
	for i, c := range calls {
		if c.First != want[i] {
			t.Errorf("rewrite #%d First = %v, want %v (all: %+v)", i+1, c.First, want[i], calls)
		}
	}
}
