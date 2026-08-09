package daemon

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// TestAgentCwdPrefersForegroundCwd pins the precedence shared by the consult,
// task-generation and learn-from-user paths: the agent may have cd'd since the
// pane was created, so what the FOREGROUND process reports wins.
func TestAgentCwdPrefersForegroundCwd(t *testing.T) {
	tests := []struct {
		name string
		info domain.PaneInfo
		want string
	}{
		{name: "foreground wins", info: domain.PaneInfo{ForegroundCwd: "/w/fg", Cwd: "/w/pane"}, want: "/w/fg"},
		{name: "pane cwd when no foreground", info: domain.PaneInfo{Cwd: "/w/pane"}, want: "/w/pane"},
		{name: "blank foreground falls through", info: domain.PaneInfo{ForegroundCwd: "  ", Cwd: "/w/pane"}, want: "/w/pane"},
		{name: "trimmed", info: domain.PaneInfo{ForegroundCwd: " /w/fg\n"}, want: "/w/fg"},
		{name: "neither reported", info: domain.PaneInfo{}, want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := agentCwd(tc.info); got != tc.want {
				t.Errorf("agentCwd(%+v) = %q, want %q", tc.info, got, tc.want)
			}
		})
	}
}

// TestConsultCarriesTheAgentsCwd drives a real consult and asserts the agent's
// working directory reaches the LLM port, which is what lets the adapter run
// the CLI inside that project (llm.run_in_agent_cwd).
func TestConsultCarriesTheAgentsCwd(t *testing.T) {
	var mu sync.Mutex
	var cwds []string
	got := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), cwds...)
	}
	// The consult errors, so every episode escalates: nothing is sent and no
	// rule is learned, which keeps the assertion about the request alone.
	h := newHarnessConsult(t, "[llm]\ncommand = [\"fake\"]\ntimeout_seconds = 5\n",
		func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
			mu.Lock()
			cwds = append(cwds, req.Cwd)
			mu.Unlock()
			return nil, errors.New("escalate")
		})
	h.herdr.mu.Lock()
	h.herdr.paneInfo = domain.PaneInfo{ForegroundCwd: "/home/op/widgets", Cwd: "/home/op"}
	h.herdr.mu.Unlock()

	h.herdr.setPane(approvalPane)
	h.push("agent-cwd-consult", "blocked")
	waitFor(t, 5*time.Second, func() bool { return len(got()) == 1 })

	if c := got()[0]; c != "/home/op/widgets" {
		t.Errorf("consult req.Cwd = %q, want the agent's foreground cwd", c)
	}
}

// TestActionReviewCarriesTheAgentsCwd covers the second of the three consult
// paths. Each one carries its own copy of the "read the cwd out of
// consultContext and pin it on the request" idiom, so dropping the line on one
// path is silent: the review still runs, just from the wrong directory.
func TestActionReviewCarriesTheAgentsCwd(t *testing.T) {
	h := approvalReviewHarness(t, "")
	h.herdr.mu.Lock()
	h.herdr.paneInfo = domain.PaneInfo{ForegroundCwd: "/home/op/widgets"}
	h.herdr.mu.Unlock()

	var calls atomic.Int32
	var cwd atomicString
	respondReview(h, &calls, 80, func(req domain.LLMRequest) (string, error) {
		cwd.set(req.Cwd)
		return "y", nil
	})

	h.push("agent-ar-cwd", "blocked")
	waitFor(t, 3*time.Second, func() bool { return calls.Load() == 1 })
	if got := cwd.get(); got != "/home/op/widgets" {
		t.Errorf("action-review req.Cwd = %q, want the agent's foreground cwd", got)
	}
}

// TestTaskListReviewCarriesTheAgentsCwd is the third consult path, for the same
// reason as the action review above.
func TestTaskListReviewCarriesTheAgentsCwd(t *testing.T) {
	h, _ := reviewHarness(t, "agent-tlr-cwd", "- [ ] 1. alpha\n", false)
	h.herdr.mu.Lock()
	h.herdr.paneInfo = domain.PaneInfo{ForegroundCwd: "/home/op/widgets"}
	h.herdr.mu.Unlock()

	var cwd atomicString
	var seen atomic.Int32
	h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		if !req.TaskReview {
			return nil, errors.New("expected a task-list review")
		}
		cwd.set(req.Cwd)
		seen.Add(1)
		return stageTaskReview(ctx, h, req, nil, "1", 90)
	}

	h.push("agent-tlr-cwd", "idle")
	waitFor(t, 5*time.Second, func() bool { return seen.Load() == 1 })
	if got := cwd.get(); got != "/home/op/widgets" {
		t.Errorf("task-list-review req.Cwd = %q, want the agent's foreground cwd", got)
	}
}

// TestConsultCwdIsEmptyWhenHerdrReportsNone covers the degrade path: the pane
// info call failing (or an adapter with no inspector surface) leaves the cwd
// empty, which the adapter reads as "run where hap runs" — the historical
// behavior, never a failed consult.
func TestConsultCwdIsEmptyWhenHerdrReportsNone(t *testing.T) {
	var mu sync.Mutex
	var calls []domain.LLMRequest
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(calls)
	}
	h := newHarnessConsult(t, "[llm]\ncommand = [\"fake\"]\ntimeout_seconds = 5\n",
		func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
			mu.Lock()
			calls = append(calls, req)
			mu.Unlock()
			return nil, errors.New("escalate")
		})
	h.herdr.mu.Lock()
	h.herdr.paneInfo = domain.PaneInfo{}
	h.herdr.mu.Unlock()

	h.herdr.setPane(approvalPane)
	h.push("agent-cwd-none", "blocked")
	waitFor(t, 5*time.Second, func() bool { return count() == 1 })

	mu.Lock()
	defer mu.Unlock()
	if calls[0].Cwd != "" {
		t.Errorf("req.Cwd = %q, want empty so the adapter falls back", calls[0].Cwd)
	}
	// The consult still ran and still escalated — an unknown directory is not
	// a reason to skip asking.
	if calls[0].RequestID == "" {
		t.Error("the consult must still be staged and run")
	}
}
