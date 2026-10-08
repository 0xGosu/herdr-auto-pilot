//go:build integration

package integration

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

// scriptedLLM answers every approval consult with a confident "Yes", staging
// the decision row in the daemon's store the way the real CLI's submit_decision
// does. Anything else is refused, so a consult the case did not expect cannot
// quietly turn into a send.
type scriptedLLM struct {
	st       *store.Store
	mu       sync.Mutex
	consults int
}

func (l *scriptedLLM) Configured() bool { return true }

func (l *scriptedLLM) Consult(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
	l.mu.Lock()
	l.consults++
	l.mu.Unlock()
	if req.SituationType != domain.SituationApproval || req.ActionReview {
		return nil, errors.New("integration test: only approval consults are scripted")
	}
	dec := domain.LLMDecision{
		RequestID: req.RequestID, Signature: req.Signature,
		SituationType: req.SituationType, AgentType: req.AgentType,
		Action: "Yes", Rationale: "a read-only tool of a fake server", ConfidentScore: 95,
		Status: "pending", CreatedAt: time.Now(),
	}
	id, err := l.st.InsertLLMDecision(ctx, dec)
	if err != nil {
		return nil, err
	}
	dec.ID = id
	return &dec, nil
}

// TestRealClaudeLLMPromotedPagedApprovalApprovesOneRequestPerAnswer drives the
// LLM-promotion send path (handleLLMOutcome) against a REAL Claude Code paged
// permission queue (#571). Every page is answered by a promoted LLM decision —
// page 1 by the startup reconcile's consult, each later page by the post-action
// self-check's capture of the page drawn in place.
//
// The invariant: every tool call Claude made is backed by an automatic audit
// row. The row is written BEFORE the keystroke (FR-024), so at no instant may
// the fake server have received more calls than there are rows — a call with
// no row behind it is a request approved by no decision, which is exactly what a
// trailing Enter after a committing digit does (#564).
func TestRealClaudeLLMPromotedPagedApprovalApprovesOneRequestPerAnswer(t *testing.T) {
	cli, pane, callLog := raisePagedApprovalQueue(t)

	const cfg = "[llm]\ncommand = [\"fake\"]\nauto_act_confidence_threshold = 50\ntimeout_seconds = 5\n" +
		"[limits]\nmax_auto_prompts_per_minute = 100\nmax_consecutive_auto_prompts = 100\n"
	h := newTestDaemonWithLLM(t, cli, cfg, func(st *store.Store) ports.LLMPort { return &scriptedLLM{st: st} })
	dctx, cancel := context.WithCancel(context.Background())
	runDaemon(t, dctx, cancel, h.Daemon)

	ctx := context.Background()
	// answers counts the automatic answers recorded for the pane, and how many
	// of them came from a promoted LLM decision.
	answers := func() (auto, llm int) {
		rows, err := h.Store.AuditLog(ctx, 200)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r.AgentID != pane || r.Status != "auto" {
				continue
			}
			auto++
			if r.Trigger == "llm-fallback" {
				llm++
			}
		}
		return auto, llm
	}
	check := func() (calls, auto, llm int) {
		got := approvedCalls(t, callLog)
		auto, llm = answers()
		if len(got) > auto {
			content, _ := cli.ReadPaneVisible(ctx, pane, 40)
			t.Fatalf("Claude ran %d tool calls %v but only %d automatic answers were recorded — "+
				"a request was approved by no decision.\npane:\n%s", len(got), got, auto, content)
		}
		return len(got), auto, llm
	}

	// Done when the queue is gone (every page answered) after at least two
	// promoted answers — page 1, and a page the self-check captured in place.
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		_, _, llm := check()
		content, _ := cli.ReadPaneVisible(ctx, pane, 60)
		if llm >= 2 && pagerPosition(content) == "" {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	// A stray approval's tool call reaches the server within moments of the key
	// (well under its 8s answer delay); give it that long to show up.
	time.Sleep(3 * time.Second)
	calls, auto, llm := check()
	if llm < 2 {
		content, _ := cli.ReadPaneVisible(ctx, pane, 40)
		t.Fatalf("want every page answered by a promoted LLM decision, got %d LLM answers (%d automatic, %d calls).\npane:\n%s",
			llm, auto, calls, content)
	}
	if calls != auto {
		t.Fatalf("Claude ran %d tool calls for %d automatic answers; every answer approves exactly one request", calls, auto)
	}
	t.Logf("%d pages answered by promoted LLM decisions, %d tool calls", llm, calls)
}
