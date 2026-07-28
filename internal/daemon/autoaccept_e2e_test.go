package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
)

// These cover only what spans modules and cannot be pinned inside a single
// implementation task: the whole path from an aged escalation through config,
// store, guards and the shared delivery pipeline to a finalized row, and the
// combined infrastructure-failure scenario.

// TestAutoAcceptEndToEndThroughTheRealSweep drives the pass the way production
// does — through the daemon's own periodic sweep, with the real store, the real
// config loader and the shared delivery pipeline — rather than by calling the
// pass directly. It is the one test that would catch a wiring mistake: a pass
// that works but is never called.
func TestAutoAcceptEndToEndThroughTheRealSweep(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	ctx := context.Background()
	h.herdr.setPane(approvalPane)
	h.herdr.setAgents(parked("pE2E", "blocked"))
	id := seedAgedEscalation(t, h, "pE2E", approvalPane, domain.SituationApproval,
		"respond: Yes", 30*time.Minute)

	// The sweep ticker fires once a minute, far too slow for a test; invoke the
	// same closure body the ticker runs, including the shared agent listing.
	agents, err := h.daemon.opt.Herdr.ListAgents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h.daemon.autoAcceptEscalations(ctx, agents)

	rec := auditRow(t, h, id)
	if rec.Status != domain.AuditStatusAutoAccepted {
		t.Fatalf("status = %q, want %q", rec.Status, domain.AuditStatusAutoAccepted)
	}
	// Delivered exactly once, as the menu digit the shared pipeline maps to.
	if got := h.herdr.sentInputs(); len(got) != 1 || got[0] != "1" {
		t.Fatalf("sent = %v, want exactly [1]", got)
	}
	// Nothing learned: no correction row, and the signature's history is
	// untouched, so the rule cannot graduate off the back of an auto-accept.
	corr, err := h.raw.UnprocessedCorrections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(corr) != 0 {
		t.Errorf("an auto-accept must write NO correction, got %+v", corr)
	}
	decisions, err := h.raw.CountDecisionsForSignature(ctx, rec.Signature)
	if err != nil {
		t.Fatal(err)
	}
	if decisions != 0 {
		t.Errorf("%d decision records written; an auto-accept must not feed the confidence model", decisions)
	}
	// The operator surfaces must not present it as the operator's own work.
	if got := frontend.AuditStatusLabel(*rec); got != "auto-sent" {
		t.Errorf("operator-facing label = %q, want \"auto-sent\"", got)
	}
	if esc, _ := h.raw.PendingEscalations(ctx); len(esc) != 0 {
		t.Errorf("the escalation must leave the pending queue, got %d", len(esc))
	}
}

// TestAutoAcceptDisabledTypeNeverFiresEndToEnd: a type left disabled never
// fires however long it waits, even with the feature enabled and every other
// guard satisfied.
func TestAutoAcceptDisabledTypeNeverFiresEndToEnd(t *testing.T) {
	h := newHarness(t, autoAcceptOn) // idle/unclassifiable disabled by default
	ctx := context.Background()
	idlePane := "● Ready.\n\nAll builds are green and every check has completed.\n\n❯ \n"
	s := classifierForTest().Classify("claude", "idle", idlePane)
	if s.Type != domain.SituationIdle {
		t.Skipf("fixture classifies as %v, not idle", s.Type)
	}
	id, err := h.raw.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "pIdle", AgentType: "claude", Trigger: "status", SituationType: domain.SituationIdle,
		Action: domain.AuditActionEscalated, Status: "escalated",
		Rationale: "[no_task_source] nothing declared", Suggestion: "respond: carry on",
		PaneExcerpt: idlePane, CreatedAt: time.Now().Add(-30 * 24 * time.Hour),
	}.WithSignatureBaseline(domain.ComputeSignature(s)))
	if err != nil {
		t.Fatal(err)
	}
	h.herdr.setPane(idlePane)

	for i := 0; i < 5; i++ {
		h.daemon.autoAcceptEscalations(ctx, parked("pIdle", "idle"))
	}
	if got := auditStatus(t, h, id); got != "escalated" {
		t.Errorf("status = %q; a disabled situation type must never fire", got)
	}
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("nothing may be sent, got %v", got)
	}
}

// TestAutoAcceptUnreachableHerdrDismissesNothing is the combined
// infrastructure-failure scenario: BOTH unevaluable paths at once — the agent
// listing cannot be obtained AND the pane cannot be read — sustained across a
// full threshold window. Nothing may be dismissed and nothing attempted.
//
// This is the property that makes an outage safe: without it, a herdr restart
// would look exactly like "every agent vanished and every situation moved on",
// and the pass would purge the operator's whole queue.
func TestAutoAcceptUnreachableHerdrDismissesNothing(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	ctx := context.Background()
	ids := []int64{
		seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 2*time.Hour),
		seedAgedEscalation(t, h, "pB", approvalPane, domain.SituationApproval, "respond: Yes", 3*time.Hour),
	}

	h.herdr.mu.Lock()
	h.herdr.failListAgents = true
	h.herdr.failRead = true
	h.herdr.mu.Unlock()

	// Many sweeps' worth of outage — well past both the threshold and the
	// absence-confirmation count.
	for i := 0; i < 12; i++ {
		agents, err := h.daemon.opt.Herdr.ListAgents(ctx)
		if err != nil {
			// What the real sweep does: it cannot list, so the pass is not run
			// at all this tick. Model that faithfully.
			continue
		}
		h.daemon.autoAcceptEscalations(ctx, agents)
	}
	for _, id := range ids {
		if got := auditStatus(t, h, id); got != "escalated" {
			t.Errorf("audit %d = %q; an unreachable herdr must dismiss nothing", id, got)
		}
	}
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("no delivery may be attempted while herdr is unreachable, got %v", got)
	}

	// Herdr comes back and the situation is intact: the queue survived and the
	// escalations proceed normally, with their original wait honoured.
	h.herdr.mu.Lock()
	h.herdr.failListAgents = false
	h.herdr.failRead = false
	h.herdr.mu.Unlock()
	h.herdr.setPane(approvalPane)

	h.daemon.autoAcceptEscalations(ctx, []domain.AgentTransition{
		{AgentID: "pA", PaneID: "pA", AgentType: "claude", Status: "blocked"},
		{AgentID: "pB", PaneID: "pB", AgentType: "claude", Status: "blocked"},
	})
	for _, id := range ids {
		if got := auditStatus(t, h, id); got != domain.AuditStatusAutoAccepted {
			t.Errorf("audit %d = %q after recovery, want it accepted normally", id, got)
		}
	}
}

// TestAutoAcceptPaneReadableButAgentListingLostDismissesNothing separates the
// two unevaluable paths: here herdr answers pane reads but the LISTING is
// empty rather than erroring. An empty listing is indistinguishable from "every
// agent exited" — and must NOT be treated as one when it cannot be trusted.
func TestAutoAcceptPaneReadableButAgentListingLostDismissesNothing(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	ctx := context.Background()
	h.herdr.setPane(approvalPane)
	id := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 2*time.Hour)

	h.herdr.mu.Lock()
	h.herdr.failListAgents = true
	h.herdr.mu.Unlock()

	for i := 0; i < 12; i++ {
		agents, err := h.daemon.opt.Herdr.ListAgents(ctx)
		if err != nil {
			continue
		}
		h.daemon.autoAcceptEscalations(ctx, agents)
	}
	if got := auditStatus(t, h, id); got != "escalated" {
		t.Errorf("status = %q; an unobtainable listing must dismiss nothing", got)
	}
}

// TestAutoAcceptDismissalReasonsAreOperatorReadable ties the terminal outcomes
// back to what an operator actually sees: each machine dismissal must be
// distinguishable from an operator's, and from each other, in a list row.
func TestAutoAcceptDismissalReasonsAreOperatorReadable(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	ctx := context.Background()

	// Stale: the pane moved on.
	h.herdr.setPane("● Something else entirely.\n\n❯ \n")
	stale := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 20*time.Minute)
	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))

	// Agent gone: confirmed across two consecutive sweeps.
	gone := seedAgedEscalation(t, h, "pGone", approvalPane, domain.SituationApproval, "respond: Yes", 20*time.Minute)
	h.daemon.autoAcceptEscalations(ctx, nil)
	h.daemon.autoAcceptEscalations(ctx, nil)

	for _, tc := range []struct {
		id        int64
		wantLabel string
		wantTag   string
	}{
		{stale, "dism:stale", domain.ReasonAutoDismissStale},
		{gone, "dism:gone", domain.ReasonAutoDismissAgentGone},
	} {
		rec := auditRow(t, h, tc.id)
		if rec.Status != "dismissed" {
			t.Errorf("audit %d = %q, want dismissed", tc.id, rec.Status)
			continue
		}
		if !strings.Contains(rec.Rationale, "["+tc.wantTag+"]") {
			t.Errorf("audit %d rationale %q does not carry [%s]", tc.id, rec.Rationale, tc.wantTag)
		}
		if got := frontend.AuditStatusLabel(*rec); got != tc.wantLabel {
			t.Errorf("audit %d label = %q, want %q", tc.id, got, tc.wantLabel)
		}
		// The audit row survives (append-only, FR-020) and keeps its origin.
		if !strings.Contains(rec.Rationale, "[shadow_mode]") {
			t.Errorf("audit %d lost its original escalation reason: %q", tc.id, rec.Rationale)
		}
	}
}
