package daemon

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// autoAcceptOn is the config that enables the feature for approval and choice
// with a 15m threshold; idle and unclassifiable stay disabled by default.
const autoAcceptOn = `
[escalations.auto_accept]
enabled = true
approval = "15m"
choice = "15m"
`

// seedAgedEscalation writes a pending escalation with a baseline that MATCHES
// what the given pane classifies to, so Guard 3 passes unless a test changes
// the pane. age back-dates created_at, which is how these tests "wait" — the
// daemon has no injectable clock and the pass derives eligibility from
// created_at.
func seedAgedEscalation(t *testing.T, h *harness, agentID, pane string,
	st domain.SituationType, suggestion string, age time.Duration) int64 {
	t.Helper()
	status := "blocked"
	if st == domain.SituationIdle {
		status = "idle"
	}
	s := classifierForTest().Classify("claude", status, pane)
	if s.Type != st {
		t.Fatalf("fixture classifies as %v, want %v", s.Type, st)
	}
	sig := domain.ComputeSignature(s)
	if sig.Verdict != domain.GuardOK {
		t.Fatalf("seed situation over-masked: %q", sig.Salient)
	}
	id, err := h.raw.AppendAudit(context.Background(), domain.AuditRecord{
		AgentID: agentID, AgentType: "claude", Trigger: "status", SituationType: st,
		Action: domain.AuditActionEscalated, Status: "escalated",
		Rationale: "[shadow_mode] learning this signature", Suggestion: suggestion,
		PaneExcerpt: pane, CreatedAt: time.Now().Add(-age),
	}.WithSignatureBaseline(sig))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func parked(agentID, status string) []domain.AgentTransition {
	return []domain.AgentTransition{
		{AgentID: agentID, PaneID: agentID, AgentType: "claude", Status: status},
	}
}

func auditStatus(t *testing.T, h *harness, id int64) string {
	t.Helper()
	rec, err := h.raw.GetAudit(context.Background(), id)
	if err != nil || rec == nil {
		t.Fatalf("GetAudit(%d): %+v %v", id, rec, err)
	}
	return rec.Status
}

func auditRow(t *testing.T, h *harness, id int64) *domain.AuditRecord {
	t.Helper()
	rec, err := h.raw.GetAudit(context.Background(), id)
	if err != nil || rec == nil {
		t.Fatalf("GetAudit(%d): %+v %v", id, rec, err)
	}
	return rec
}

// TestAutoAcceptDeliversAgedEscalation is the happy path: an escalation past
// its threshold, on a live parked agent still showing the same screen, is
// delivered and finalized — and CRUCIALLY writes no correction, so nothing is
// learned from a machine's decision to stop waiting.
func TestAutoAcceptDeliversAgedEscalation(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	ctx := context.Background()
	h.herdr.setPane(approvalPane)
	id := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 20*time.Minute)

	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))

	if got := auditStatus(t, h, id); got != domain.AuditStatusAutoAccepted {
		t.Fatalf("status = %q, want %q", got, domain.AuditStatusAutoAccepted)
	}
	// The numbered menu wants the DIGIT, not the label — proof it went through
	// the shared delivery pipeline rather than a naive literal send.
	if got := h.herdr.sentInputs(); len(got) != 1 || got[0] != "1" {
		t.Errorf("sent = %v, want [1]", got)
	}
	// No learning event, no confidence contribution, no graduation progress.
	corr, err := h.raw.UnprocessedCorrections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(corr) != 0 {
		t.Errorf("an auto-accept must write NO correction, got %+v", corr)
	}
	if esc, _ := h.raw.PendingEscalations(ctx); len(esc) != 0 {
		t.Errorf("the escalation must leave the pending queue, got %d", len(esc))
	}
}

// TestAutoAcceptDisabledDoesNothing: the feature is off by default, and a
// disabled situation type never fires however long it waits.
func TestAutoAcceptDisabledDoesNothing(t *testing.T) {
	tests := []struct {
		name string
		cfg  string
	}{
		{"absent section", ""},
		{"explicitly disabled", "[escalations.auto_accept]\nenabled = false\napproval = \"1m\"\n"},
		{"type disabled", "[escalations.auto_accept]\nenabled = true\napproval = \"0\"\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, tt.cfg)
			ctx := context.Background()
			h.herdr.setPane(approvalPane)
			id := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval,
				"respond: Yes", 10*time.Hour)

			h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))

			if got := auditStatus(t, h, id); got != "escalated" {
				t.Errorf("status = %q, want it left pending", got)
			}
			if got := h.herdr.sentInputs(); len(got) != 0 {
				t.Errorf("nothing may be sent, got %v", got)
			}
		})
	}
}

// TestAutoAcceptBelowThresholdWaits: threshold arithmetic. An escalation that
// has not yet waited long enough is untouched.
func TestAutoAcceptBelowThresholdWaits(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	ctx := context.Background()
	h.herdr.setPane(approvalPane)
	id := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 5*time.Minute)

	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))

	if got := auditStatus(t, h, id); got != "escalated" {
		t.Errorf("status = %q, want it still pending below its threshold", got)
	}
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("nothing may be sent, got %v", got)
	}
}

// TestAutoAcceptKillSwitchDismissesNothing: pausing the herd must never destroy
// the queue it protects. Held active over a full threshold window, the pass
// neither sends nor dismisses — and the escalation keeps its original
// created_at, so releasing the switch resumes with the wait intact.
func TestAutoAcceptKillSwitchDismissesNothing(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	ctx := context.Background()
	// The pane has MOVED ON, so without the kill switch this would be dismissed
	// as stale — proving the kill switch short-circuits the dismissal path too.
	h.herdr.setPane("● All done. Nothing to see here.\n")
	id := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 20*time.Minute)
	before := auditRow(t, h, id).CreatedAt

	if _, err := h.raw.InsertKillEvent(ctx, domain.KillEvent{
		State: "active", Scope: "global", Author: "operator", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 5; i++ {
		h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))
	}
	if got := auditStatus(t, h, id); got != "escalated" {
		t.Fatalf("status = %q; a paused herd must dismiss nothing", got)
	}
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("nothing may be sent while paused, got %v", got)
	}
	if got := auditRow(t, h, id).CreatedAt; !got.Equal(before) {
		t.Errorf("created_at changed under the kill switch: %v -> %v", before, got)
	}
}

// TestAutoAcceptSkipsPausedAndDisabledAgents: both are operator controls that
// SUPPRESS, never retire — a temporarily paused agent must not lose its queue.
func TestAutoAcceptSkipsPausedAndDisabledAgents(t *testing.T) {
	t.Run("paused agent", func(t *testing.T) {
		h := newHarness(t, autoAcceptOn)
		ctx := context.Background()
		h.herdr.setPane(approvalPane)
		id := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 20*time.Minute)
		if err := h.raw.UpdateAgentRate(ctx, domain.AgentRate{AgentID: "pA", Paused: true}); err != nil {
			t.Fatal(err)
		}

		h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))

		if got := auditStatus(t, h, id); got != "escalated" {
			t.Errorf("status = %q, want left pending", got)
		}
		if got := h.herdr.sentInputs(); len(got) != 0 {
			t.Errorf("nothing may be sent to a paused agent, got %v", got)
		}
	})
	t.Run("disabled agent", func(t *testing.T) {
		h := newHarness(t, autoAcceptOn)
		ctx := context.Background()
		h.herdr.setPane(approvalPane)
		id := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 20*time.Minute)
		if _, err := h.raw.EnsureAgentName(ctx, "pA"); err != nil {
			t.Fatal(err)
		}
		if err := h.raw.SetAgentDisabled(ctx, "pA", true); err != nil {
			t.Fatal(err)
		}

		h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))

		if got := auditStatus(t, h, id); got != "escalated" {
			t.Errorf("status = %q, want left pending", got)
		}
		if got := h.herdr.sentInputs(); len(got) != 0 {
			t.Errorf("nothing may be sent to a disabled agent, got %v", got)
		}
	})
}

// TestAutoAcceptGuard2RejectsBusyAgents: a working or freshly detected agent is
// skipped before any pane read, and skipping is transient — never a dismissal.
func TestAutoAcceptGuard2RejectsBusyAgents(t *testing.T) {
	for _, status := range []string{"working", "detected", "unknown"} {
		t.Run(status, func(t *testing.T) {
			h := newHarness(t, autoAcceptOn)
			ctx := context.Background()
			h.herdr.setPane(approvalPane)
			id := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 20*time.Minute)
			readsBefore := len(h.herdr.readLineCalls())

			h.daemon.autoAcceptEscalations(ctx, parked("pA", status))

			if got := auditStatus(t, h, id); got != "escalated" {
				t.Errorf("status = %q, want left pending", got)
			}
			if len(h.herdr.readLineCalls()) != readsBefore {
				t.Error("Guard 2 must reject before any pane read")
			}
		})
	}
}

// TestAutoAcceptGuard2AdmitsParkedStatuses: blocked, idle and done all pass —
// a done -> idle flip during the wait is exactly the transition the dedup
// window exists to absorb, so it must not block an auto-accept.
func TestAutoAcceptGuard2AdmitsParkedStatuses(t *testing.T) {
	for _, status := range []string{"blocked", "idle", "done"} {
		t.Run(status, func(t *testing.T) {
			h := newHarness(t, autoAcceptOn)
			ctx := context.Background()
			h.herdr.setPane(approvalPane)
			id := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 20*time.Minute)

			h.daemon.autoAcceptEscalations(ctx, parked("pA", status))

			if got := auditStatus(t, h, id); got != domain.AuditStatusAutoAccepted {
				t.Errorf("status = %q, want %q for a parked agent", got, domain.AuditStatusAutoAccepted)
			}
		})
	}
}

// TestAutoAcceptGuard3DismissesStaleSituation: the pane moved on, so the
// suggestion would answer a question nobody asked. Dismissed with the reason
// recorded, and nothing delivered.
func TestAutoAcceptGuard3DismissesStaleSituation(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	ctx := context.Background()
	h.herdr.setPane("● Working on something else entirely now.\n\n❯ \n")
	id := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 20*time.Minute)

	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))

	rec := auditRow(t, h, id)
	if rec.Status != "dismissed" {
		t.Fatalf("status = %q, want dismissed", rec.Status)
	}
	if got := domain.AutoDismissReason(rec.Rationale); got != domain.ReasonAutoDismissStale {
		t.Errorf("rationale %q carries reason %q, want %q", rec.Rationale, got, domain.ReasonAutoDismissStale)
	}
	// The original escalation reason must survive alongside the machine's.
	if !strings.Contains(rec.Rationale, "[shadow_mode]") {
		t.Errorf("the original reason was lost: %q", rec.Rationale)
	}
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("nothing may be delivered when a guard fails, got %v", got)
	}
}

// TestAutoAcceptGuard3StructuredSalientRequiresExactMatch is the
// structured-salient carve-out. An approval's salient is its permission verb
// plus its OPTION SET — short and identity-bearing, so a one-word difference
// leaves most of its trigrams intact and an order-insensitive similarity
// compare would happily accept a materially different screen.
// SignatureHeldStill therefore keeps structured salients on EXACT matching,
// and approval/choice/error — the three types enabled by default — are all
// structured. Here the option set changed, so the compare must refuse.
//
// NOTE what this test does NOT claim. Two approvals differing only in the
// COMMAND ("Bash(go test)" vs "Bash(npm install)") hash IDENTICALLY today: the
// salient carries the verb and options, never the command. They are one
// learned rule throughout the system, so auto-accept treating them alike is
// consistent with every other path rather than a new exposure.
func TestAutoAcceptGuard3StructuredSalientRequiresExactMatch(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	ctx := context.Background()
	// A different question with a DIFFERENT option set is now on screen.
	differentOptions := "Bash(rm -rf build)\n\nDo you want to proceed?\n❯ 1. Yes\n  2. Yes, and don't ask again\n  3. No\n"
	h.herdr.setPane(differentOptions)
	id := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 20*time.Minute)

	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))

	rec := auditRow(t, h, id)
	if rec.Status != "dismissed" {
		t.Fatalf("status = %q: a different option set must NOT be auto-accepted "+
			"(the structured-salient carve-out did not hold)", rec.Status)
	}
	if got := domain.AutoDismissReason(rec.Rationale); got != domain.ReasonAutoDismissStale {
		t.Errorf("reason = %q, want %q", got, domain.ReasonAutoDismissStale)
	}
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("nothing may be delivered, got %v", got)
	}
}

// TestAutoAcceptGuard3AcceptsARepaintedPaneTail is the other direction of the
// jitter tolerance. An UNSTRUCTURED pane-tail salient (an idle situation) drifts
// as the agent TUI repaints its status line; a byte-exact compare would reject
// screens that did hold still, so the tolerance must absorb that.
func TestAutoAcceptGuard3AcceptsARepaintedPaneTail(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	ctx := context.Background()
	h.writeConfig(t, autoAcceptOn+"\nidle = \"15m\"\n")
	h.daemon.reload()

	// Varied lines, not a repeated one: the comparison is a trigram Jaccard, so
	// highly repetitive content collapses to a tiny distinct-trigram set in
	// which a handful of changed characters dominates. Real pane output is
	// varied; a repeated fixture would fail for a reason the tolerance is not
	// about.
	body := ""
	for _, w := range []string{"parsing manifest", "resolving imports", "linking objects",
		"vetting handlers", "running migrations", "checking schemas", "loading fixtures",
		"building indexes", "warming caches", "flushing buffers", "closing sockets",
		"writing artifacts"} {
		body += "○ " + w + " finished successfully in the background\n"
	}
	// Only the status line differs — the spinner's elapsed time and token count.
	before := body + "● Idling… (12s · 3.1k tokens)\n\n❯ \n"
	after := body + "● Idling… (47s · 3.4k tokens)\n\n❯ \n"

	s := classifierForTest().Classify("claude", "idle", before)
	if s.Type != domain.SituationIdle {
		t.Fatalf("fixture classifies as %v, want idle", s.Type)
	}
	sig := domain.ComputeSignature(s)
	if domain.StructuredSalient(sig.Salient) {
		t.Fatalf("fixture must produce an UNSTRUCTURED pane-tail salient, got %q", sig.Salient)
	}
	id, err := h.raw.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "pA", AgentType: "claude", Trigger: "status", SituationType: domain.SituationIdle,
		Action: domain.AuditActionEscalated, Status: "escalated",
		Rationale: "[shadow_mode] learning", Suggestion: "respond: continue",
		PaneExcerpt: before, CreatedAt: time.Now().Add(-20 * time.Minute),
	}.WithSignatureBaseline(sig))
	if err != nil {
		t.Fatal(err)
	}

	h.herdr.setPane(after)
	h.daemon.autoAcceptEscalations(ctx, parked("pA", "idle"))

	if got := auditStatus(t, h, id); got == "dismissed" {
		t.Error("a status-line repaint within tolerance was read as stale")
	}
}

// TestAutoAcceptUnreadablePaneDismissesNothing: an unevaluable guard is ALWAYS
// transient. Herdr unreachable for any duration retires nothing — absence of
// evidence is never evidence of staleness.
func TestAutoAcceptUnreadablePaneDismissesNothing(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	ctx := context.Background()
	id := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 20*time.Minute)
	h.herdr.mu.Lock()
	h.herdr.failRead = true
	h.herdr.mu.Unlock()

	for i := 0; i < 10; i++ {
		h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))
	}
	if got := auditStatus(t, h, id); got != "escalated" {
		t.Errorf("status = %q; an unreadable pane must dismiss nothing", got)
	}
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("nothing may be sent, got %v", got)
	}
}

// TestAutoAcceptAgentAbsenceNeedsConfirmation: an agent missing from the
// listing is terminal, but only once confirmed across consecutive sweeps — an
// incomplete listing during a herdr restart must not purge live escalations.
func TestAutoAcceptAgentAbsenceNeedsConfirmation(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	ctx := context.Background()
	h.herdr.setPane(approvalPane)
	id := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 20*time.Minute)

	// First sweep with the agent missing: observed, not acted on.
	h.daemon.autoAcceptEscalations(ctx, nil)
	if got := auditStatus(t, h, id); got != "escalated" {
		t.Fatalf("status = %q after ONE absence; absence must be confirmed first", got)
	}
	// Second consecutive absence confirms it.
	h.daemon.autoAcceptEscalations(ctx, nil)
	rec := auditRow(t, h, id)
	if rec.Status != "dismissed" {
		t.Fatalf("status = %q after two absences, want dismissed", rec.Status)
	}
	if got := domain.AutoDismissReason(rec.Rationale); got != domain.ReasonAutoDismissAgentGone {
		t.Errorf("reason = %q, want %q", got, domain.ReasonAutoDismissAgentGone)
	}
}

// TestAutoAcceptAgentReappearanceClearsAbsence: the run must be CONSECUTIVE.
func TestAutoAcceptAgentReappearanceClearsAbsence(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	ctx := context.Background()
	// The pane has moved on, so a reappearance resolves to a stale dismissal
	// rather than a delivery — either way it must not be "agent gone".
	h.herdr.setPane("● Busy with something else.\n")
	id := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 20*time.Minute)

	h.daemon.autoAcceptEscalations(ctx, nil)                     // absent once
	h.daemon.autoAcceptEscalations(ctx, parked("pA", "working")) // back, but busy
	if got := auditStatus(t, h, id); got != "escalated" {
		t.Fatalf("status = %q; a reappearance must clear the absence observation", got)
	}
	// Absent again: this is the FIRST of a new run, not the second of the old.
	h.daemon.autoAcceptEscalations(ctx, nil)
	if got := auditStatus(t, h, id); got != "escalated" {
		rec := auditRow(t, h, id)
		t.Fatalf("status = %q (%q); the absence run must have restarted", got, rec.Rationale)
	}
}

// TestAutoAcceptOnePerAgentPerTick: two aged escalations on one agent must
// never both deliver into the same pane in one sweep.
func TestAutoAcceptOnePerAgentPerTick(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	ctx := context.Background()
	h.herdr.setPane(approvalPane)
	older := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 40*time.Minute)
	newer := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 20*time.Minute)

	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))

	if got := len(h.herdr.sentInputs()); got != 1 {
		t.Fatalf("%d deliveries in one tick, want exactly 1", got)
	}
	// Oldest first: the longest-waiting escalation is the one taken.
	if got := auditStatus(t, h, older); got != domain.AuditStatusAutoAccepted {
		t.Errorf("the OLDER escalation should have been taken first, got %q", got)
	}
	if got := auditStatus(t, h, newer); got != "escalated" {
		t.Errorf("the newer escalation = %q, want deferred to a later tick", got)
	}

	// The next tick picks up the deferred one.
	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))
	if got := auditStatus(t, h, newer); got != domain.AuditStatusAutoAccepted {
		t.Errorf("second tick: newer = %q, want accepted", got)
	}
}

// TestAutoAcceptSeparateAgentsBothProceed: the one-per-tick bound is per AGENT,
// not global.
func TestAutoAcceptSeparateAgentsBothProceed(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	ctx := context.Background()
	h.herdr.setPane(approvalPane)
	a := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 20*time.Minute)
	b := seedAgedEscalation(t, h, "pB", approvalPane, domain.SituationApproval, "respond: Yes", 20*time.Minute)

	h.daemon.autoAcceptEscalations(ctx, []domain.AgentTransition{
		{AgentID: "pA", PaneID: "pA", AgentType: "claude", Status: "blocked"},
		{AgentID: "pB", PaneID: "pB", AgentType: "claude", Status: "blocked"},
	})

	if got := auditStatus(t, h, a); got != domain.AuditStatusAutoAccepted {
		t.Errorf("pA = %q, want accepted", got)
	}
	if got := auditStatus(t, h, b); got != domain.AuditStatusAutoAccepted {
		t.Errorf("pB = %q, want accepted", got)
	}
}

// TestAutoAcceptRetriesThenGivesUp: a delivery that keeps failing is reverted
// to the queue and retried a bounded number of times, then retired visibly
// rather than re-running the whole guard chain every minute forever.
func TestAutoAcceptRetriesThenGivesUp(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	ctx := context.Background()
	h.herdr.setPane(approvalPane)
	h.herdr.mu.Lock()
	h.herdr.failSend = true
	h.herdr.mu.Unlock()
	id := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 20*time.Minute)

	for i := 1; i < maxAutoAcceptAttempts; i++ {
		h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))
		if got := auditStatus(t, h, id); got != "escalated" {
			t.Fatalf("attempt %d: status = %q, want it returned to the queue", i, got)
		}
	}
	// The attempt that exhausts the budget retires it.
	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))
	rec := auditRow(t, h, id)
	if rec.Status != "dismissed" {
		t.Fatalf("status = %q after exhausting attempts, want dismissed", rec.Status)
	}
	if got := domain.AutoDismissReason(rec.Rationale); got != domain.ReasonAutoAcceptFailed {
		t.Errorf("reason = %q, want %q", got, domain.ReasonAutoAcceptFailed)
	}
	// The counter must not outlive the escalation.
	h.daemon.mu.Lock()
	_, leaked := h.daemon.autoAcceptAttempts[id]
	h.daemon.mu.Unlock()
	if leaked {
		t.Error("the attempt counter leaked after the escalation reached a terminal status")
	}
}

// TestAutoAcceptAttemptBudgetIsNotPersisted: a restart re-grants a fresh
// budget, because a restart is itself a plausible fix for a transient fault.
func TestAutoAcceptAttemptBudgetIsNotPersisted(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	ctx := context.Background()
	h.herdr.setPane(approvalPane)
	h.herdr.mu.Lock()
	h.herdr.failSend = true
	h.herdr.mu.Unlock()
	id := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 20*time.Minute)

	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))
	h.daemon.mu.Lock()
	got := h.daemon.autoAcceptAttempts[id]
	h.daemon.mu.Unlock()
	if got != 1 {
		t.Fatalf("attempts = %d, want 1", got)
	}
	// Nothing about the count is durable: the row carries no attempt state.
	rec := auditRow(t, h, id)
	if strings.Contains(rec.Rationale, "attempt") {
		t.Errorf("attempt bookkeeping leaked into the durable row: %q", rec.Rationale)
	}
}

// TestAutoAcceptSweepDemotedEscalationIsLeftPending: escalations raised by the
// multi-tab sweep carry a bare error string with no "[reason]" tag. The
// fail-closed unparseable-reason rule excludes them — and must LEAVE them
// pending, not dismiss them.
func TestAutoAcceptSweepDemotedEscalationIsLeftPending(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	ctx := context.Background()
	h.herdr.setPane(approvalPane)
	id := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 20*time.Minute)
	// What escalateAudit writes: a bare err.Error(), no tag.
	if err := h.raw.EscalateAudit(ctx, id, "per-tab select kinds changed since capture", "answer series: 1 2 1"); err != nil {
		t.Fatal(err)
	}

	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))

	if got := auditStatus(t, h, id); got != "escalated" {
		t.Errorf("status = %q; a sweep-demoted escalation must stay pending for the operator", got)
	}
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("nothing may be re-sent against a drifted form, got %v", got)
	}
}

// TestAutoAcceptNeverAutoEscalationIsNeverAccepted: FR-015's invariant. A
// never-auto match always reaches a human; a timeout is not a human. It is
// left pending, not dismissed.
func TestAutoAcceptNeverAutoEscalationIsNeverAccepted(t *testing.T) {
	for _, reason := range []string{"never_auto_match", "suspected_irreversible"} {
		t.Run(reason, func(t *testing.T) {
			h := newHarness(t, autoAcceptOn)
			ctx := context.Background()
			h.herdr.setPane(approvalPane)
			id := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 10*time.Hour)
			if err := h.raw.EscalateAudit(ctx, id, "["+reason+"] matches a destructive pattern", "respond: Yes"); err != nil {
				t.Fatal(err)
			}

			for i := 0; i < 5; i++ {
				h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))
			}
			if got := auditStatus(t, h, id); got != "escalated" {
				t.Errorf("status = %q; a hard-safety escalation must stay for the operator", got)
			}
			if got := h.herdr.sentInputs(); len(got) != 0 {
				t.Errorf("nothing may be sent, got %v", got)
			}
		})
	}
}

// TestAutoAcceptNoBaselineFailsClosed: the entire pre-upgrade backlog looks
// like this. It stays operator-only and is never dismissed.
func TestAutoAcceptNoBaselineFailsClosed(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	ctx := context.Background()
	h.herdr.setPane(approvalPane)
	id, err := h.raw.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "pA", AgentType: "claude", Trigger: "status",
		SituationType: domain.SituationApproval, Action: domain.AuditActionEscalated,
		Status: "escalated", Rationale: "[shadow_mode] learning", Suggestion: "respond: Yes",
		PaneExcerpt: approvalPane, CreatedAt: time.Now().Add(-10 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))

	if got := auditStatus(t, h, id); got != "escalated" {
		t.Errorf("status = %q; a baseline-less row must fail closed and stay pending", got)
	}
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("nothing may be sent, got %v", got)
	}
}

// TestAutoAcceptRespectsAPaneSalientCharsChange: fresh is computed with the
// ROW's stored salient window, not the currently configured one, so an
// operator editing embedding.pane_salient_chars mid-wait cannot shift the
// comparison basis and manufacture a spurious staleness.
func TestAutoAcceptRespectsAPaneSalientCharsChange(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	ctx := context.Background()
	// An idle pane-tail situation, whose salient IS the trailing window — the
	// only shape a salient-chars change can affect.
	idlePane := strings.Repeat("routine build output line\n", 60) + "\n❯ \n"
	h.writeConfig(t, autoAcceptOn+"\nidle = \"15m\"\n[embedding]\npane_salient_chars = 400\n")
	h.daemon.reload()

	s := classifierForTest().Classify("claude", "idle", idlePane)
	sig := domain.ComputeSignatureN(s, 400)
	id, err := h.raw.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "pA", AgentType: "claude", Trigger: "status", SituationType: s.Type,
		Action: domain.AuditActionEscalated, Status: "escalated",
		Rationale: "[shadow_mode] learning", Suggestion: "respond: continue",
		PaneExcerpt: idlePane, CreatedAt: time.Now().Add(-20 * time.Minute),
	}.WithSignatureBaseline(sig))
	if err != nil {
		t.Fatal(err)
	}
	if sig.SalientChars != 400 {
		t.Fatalf("baseline recorded SalientChars=%d, want the 400 in effect", sig.SalientChars)
	}

	// The operator now WIDENS the window. The pane is unchanged.
	h.writeConfig(t, autoAcceptOn+"\nidle = \"15m\"\n[embedding]\npane_salient_chars = 1200\n")
	h.daemon.reload()
	h.herdr.setPane(idlePane)

	h.daemon.autoAcceptEscalations(ctx, parked("pA", "idle"))

	if got := auditStatus(t, h, id); got == "dismissed" {
		t.Error("an unchanged pane read as stale: fresh was computed with the LIVE " +
			"salient window instead of the row's, shifting the comparison basis")
	}
}

// TestAutoAcceptLosesClaimToOperatorSilently: a concurrent operator confirm and
// an auto-accept must produce exactly ONE delivery. The claim is atomic, so the
// loser skips without sending.
func TestAutoAcceptLosesClaimToOperatorSilently(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	ctx := context.Background()
	h.herdr.setPane(approvalPane)
	id := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 20*time.Minute)

	// The operator resolves it first — exactly what winning the race looks like
	// from the daemon's side.
	if ok, err := h.raw.ResolveEscalation(ctx, id); err != nil || !ok {
		t.Fatalf("resolve: %v %v", ok, err)
	}

	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))

	if got := auditStatus(t, h, id); got != "resolved" {
		t.Errorf("status = %q; the operator's resolution must stand", got)
	}
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("the losing writer must not deliver, got %v", got)
	}
}

// TestAutoAcceptClaimRaceDeliversOnce drives the race directly: many goroutines
// claiming one row, only the winner may send.
func TestAutoAcceptClaimRaceDeliversOnce(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	ctx := context.Background()
	h.herdr.setPane(approvalPane)
	id := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 20*time.Minute)

	var wg sync.WaitGroup
	wins := make([]bool, 6)
	for i := range wins {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, _ := h.raw.ClaimForAutoAccept(ctx, id)
			wins[i] = ok
		}(i)
	}
	wg.Wait()
	won := 0
	for _, w := range wins {
		if w {
			won++
		}
	}
	if won != 1 {
		t.Fatalf("%d claimants won, want exactly 1 — more than one would double-send", won)
	}
}

// TestAutoAcceptStartupReclaimReturnsAbandonedClaims: a row left mid-delivery
// by a crash returns to the pending queue and is re-evaluated against the whole
// guard chain.
func TestAutoAcceptStartupReclaimReturnsAbandonedClaims(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	ctx := context.Background()
	h.herdr.setPane(approvalPane)
	id := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 20*time.Minute)

	// Simulate the crash window: claimed, never finalized.
	if ok, err := h.raw.ClaimForAutoAccept(ctx, id); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	// While abandoned it is invisible to BOTH sides — the failure the transient
	// status exists to make recoverable.
	if esc, _ := h.raw.PendingEscalations(ctx); len(esc) != 0 {
		t.Fatalf("an abandoned claim must not appear pending, got %d", len(esc))
	}
	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))
	if got := auditStatus(t, h, id); got != domain.AuditStatusAutoAccepting {
		t.Fatalf("status = %q; an abandoned claim is not a candidate until reclaimed", got)
	}

	n, err := h.raw.ReclaimAbandonedAutoAccepts(ctx)
	if err != nil || n != 1 {
		t.Fatalf("reclaim = (%d, %v), want (1, nil)", n, err)
	}
	if got := auditStatus(t, h, id); got != "escalated" {
		t.Fatalf("status = %q after reclaim, want escalated", got)
	}

	// Re-evaluated from scratch, and the pane still matches, so it delivers.
	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))
	if got := auditStatus(t, h, id); got != domain.AuditStatusAutoAccepted {
		t.Errorf("status = %q, want the reclaimed row to be re-evaluated and delivered", got)
	}
}

// TestAutoAcceptReclaimAfterALandedDeliveryIsCaughtByGuard3 is the ambiguous
// crash case: the send DID land, then the daemon died before finalizing.
// Reclaiming makes the row a candidate again — and it must not be answered
// twice. Guard 3 is the backstop: the delivered reply changed the pane.
func TestAutoAcceptReclaimAfterALandedDeliveryIsCaughtByGuard3(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	ctx := context.Background()
	h.herdr.setPane(approvalPane)
	id := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 20*time.Minute)

	if ok, _ := h.raw.ClaimForAutoAccept(ctx, id); !ok {
		t.Fatal("claim failed")
	}
	// The reply landed: the approval is gone and the agent moved on.
	h.herdr.setPane("● Running tests…\n\n❯ \n")
	if n, err := h.raw.ReclaimAbandonedAutoAccepts(ctx); err != nil || n != 1 {
		t.Fatalf("reclaim = (%d, %v)", n, err)
	}

	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))

	rec := auditRow(t, h, id)
	if rec.Status != "dismissed" {
		t.Fatalf("status = %q; a reclaimed row whose reply LANDED must not be answered twice", rec.Status)
	}
	if got := domain.AutoDismissReason(rec.Rationale); got != domain.ReasonAutoDismissStale {
		t.Errorf("reason = %q, want %q", got, domain.ReasonAutoDismissStale)
	}
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("the reply must NOT be sent a second time, got %v", got)
	}
}

// TestAutoAcceptDoesNotCountAgainstRateLimits: an auto-accept answers a
// question the AGENT asked, so it is not an unsolicited auto-prompt and must
// not advance the runaway counters (which would eventually pause the agent and
// silently stop the feature).
func TestAutoAcceptDoesNotCountAgainstRateLimits(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	ctx := context.Background()
	h.herdr.setPane(approvalPane)
	before, err := h.raw.GetAgentRate(ctx, "pA")
	if err != nil {
		t.Fatal(err)
	}
	seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 20*time.Minute)

	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))

	after, err := h.raw.GetAgentRate(ctx, "pA")
	if err != nil {
		t.Fatal(err)
	}
	if after.ConsecutiveAuto != before.ConsecutiveAuto {
		t.Errorf("consecutive counter advanced %d -> %d; an auto-accept is not an auto-prompt",
			before.ConsecutiveAuto, after.ConsecutiveAuto)
	}
	if after.Paused {
		t.Error("an auto-accept must never pause its own agent")
	}
}

// TestAutoAcceptStatePrunedWhenEscalationsLeaveTheEligibleSet: the in-memory
// maps stay bounded by the eligible set, not by daemon uptime.
func TestAutoAcceptStatePrunedWhenEscalationsLeaveTheEligibleSet(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	ctx := context.Background()
	h.herdr.setPane(approvalPane)
	h.herdr.mu.Lock()
	h.herdr.failSend = true
	h.herdr.mu.Unlock()
	id := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 20*time.Minute)

	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))
	h.daemon.mu.Lock()
	tracked := len(h.daemon.autoAcceptAttempts)
	h.daemon.mu.Unlock()
	if tracked != 1 {
		t.Fatalf("tracked %d attempts, want 1", tracked)
	}

	// The operator resolves it: the id leaves the eligible set.
	if ok, _ := h.raw.ResolveEscalation(ctx, id); !ok {
		t.Fatal("resolve failed")
	}
	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))

	h.daemon.mu.Lock()
	tracked = len(h.daemon.autoAcceptAttempts)
	h.daemon.mu.Unlock()
	if tracked != 0 {
		t.Errorf("%d entries survived after leaving the eligible set; the map is unbounded", tracked)
	}
}

// TestAutoAcceptNeverTypesTheNoopSentinel: "@noop" is a sentinel meaning "send
// nothing". The delivery pipeline deliberately treats it as ordinary text, so
// without an explicit gate an auto-accept would type "@noop" into the agent.
func TestAutoAcceptNeverTypesTheNoopSentinel(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	ctx := context.Background()
	h.herdr.setPane(approvalPane)
	// The spelling the LLM-reject path writes, which SuggestedAction resolves
	// to the sentinel.
	id := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval,
		domain.ActionNoopSuggestion, 20*time.Minute)

	for i := 0; i < 3; i++ {
		h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))
	}
	for _, got := range h.herdr.sentInputs() {
		if strings.Contains(got, "@noop") {
			t.Fatalf("the noop SENTINEL was typed at the agent: %q", got)
		}
	}
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("a noop suggestion means send nothing, got %v", got)
	}
	if got := auditStatus(t, h, id); got != "escalated" {
		t.Errorf("status = %q, want it left for the operator", got)
	}
}

// TestAutoAcceptNeverReSendsAnExhaustedRetry: FR-014's ceiling. Auto-accepting
// a retry_exhausted escalation would re-send the very retry the ceiling exists
// to stop — and because an auto-accept writes no correction and is not counted
// against [limits], the counter never advances, so the loop would repeat every
// threshold window forever with nobody watching.
func TestAutoAcceptNeverReSendsAnExhaustedRetry(t *testing.T) {
	for _, reason := range []string{"retry_exhausted", "rate_limited"} {
		t.Run(reason, func(t *testing.T) {
			h := newHarness(t, autoAcceptOn)
			ctx := context.Background()
			h.herdr.setPane(approvalPane)
			id := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval,
				"respond: Yes", 10*time.Hour)
			if err := h.raw.EscalateAudit(ctx, id, "["+reason+"] ceiling reached", "respond: Yes"); err != nil {
				t.Fatal(err)
			}

			for i := 0; i < 5; i++ {
				h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))
			}
			if got := auditStatus(t, h, id); got != "escalated" {
				t.Errorf("status = %q; a ceiling verdict must stay for the operator", got)
			}
			if got := h.herdr.sentInputs(); len(got) != 0 {
				t.Errorf("nothing may be sent, got %v", got)
			}
		})
	}
}

// TestAutoAcceptReadsEachPaneOncePerTick: the pass runs on the daemon's select
// loop and each pane read shells out to herdr. Every escalation on an agent
// looks at the SAME screen, so re-reading per candidate would stall the loop
// for no new information.
func TestAutoAcceptReadsEachPaneOncePerTick(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	ctx := context.Background()
	// A pane that has moved on, so no candidate is consumed by a delivery and
	// every one of them reaches Guard 3.
	h.herdr.setPane("● Elsewhere now.\n\n❯ \n")
	for i := 0; i < 4; i++ {
		seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval,
			"respond: Yes", time.Duration(20+i)*time.Minute)
	}
	before := len(h.herdr.readLineCalls())

	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))

	if reads := len(h.herdr.readLineCalls()) - before; reads > 1 {
		t.Errorf("%d pane reads for one agent in one tick, want 1", reads)
	}
}

// TestAutoAcceptUnstructuredMismatchLeavesPending: a pane-tail salient cannot
// be compared confidently here — the baseline was minted from a consuming
// "recent" read while the guard re-reads the visible screen, so the two hash
// differently even when nothing changed. A comparison known to be unreliable
// must never DESTROY a queue entry: leave it pending rather than dismiss it.
func TestAutoAcceptUnstructuredMismatchLeavesPending(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	ctx := context.Background()
	h.writeConfig(t, autoAcceptOn+"\nidle = \"15m\"\n")
	h.daemon.reload()

	before := "● Ready.\n\nEvery check has completed and the working tree is clean.\n\n❯ \n"
	s := classifierForTest().Classify("claude", "idle", before)
	if s.Type != domain.SituationIdle {
		t.Skipf("fixture classifies as %v, not idle", s.Type)
	}
	sig := domain.ComputeSignature(s)
	if domain.StructuredSalient(sig.Salient) {
		t.Skip("fixture is not an unstructured pane-tail salient")
	}
	id, err := h.raw.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "pA", AgentType: "claude", Trigger: "status", SituationType: domain.SituationIdle,
		Action: domain.AuditActionEscalated, Status: "escalated",
		Rationale: "[no_task_source] nothing declared", Suggestion: "respond: carry on",
		PaneExcerpt: before, CreatedAt: time.Now().Add(-20 * time.Minute),
	}.WithSignatureBaseline(sig))
	if err != nil {
		t.Fatal(err)
	}
	// A completely different idle screen — still idle, but nothing alike.
	h.herdr.setPane("● Ready.\n\nWaiting for instructions after a long and unrelated build.\n\n❯ \n")

	for i := 0; i < 3; i++ {
		h.daemon.autoAcceptEscalations(ctx, parked("pA", "idle"))
	}
	if got := auditStatus(t, h, id); got == "dismissed" {
		t.Error("an unstructured salient mismatch DESTROYED the queue entry; " +
			"the comparison is known to be unreliable for pane-tail salients, so it must leave it pending")
	}
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("nor may it deliver on an unreliable comparison, got %v", got)
	}
}

// TestAutoAcceptWithholdsDeliveredAgentsFromTheRestOfTheSweep: clearing the
// escalation is exactly what stops the later passes skipping that agent, so
// without this a parked agent could take the auto-accepted reply AND an idle
// task hand-out in the same tick, before its TUI had repainted.
func TestAutoAcceptWithholdsDeliveredAgentsFromTheRestOfTheSweep(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	ctx := context.Background()
	h.herdr.setPane(approvalPane)
	seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 20*time.Minute)

	agents := []domain.AgentTransition{
		{AgentID: "pA", PaneID: "pA", AgentType: "claude", Status: "blocked"},
		{AgentID: "pB", PaneID: "pB", AgentType: "claude", Status: "idle"},
	}
	delivered := h.daemon.autoAcceptEscalations(ctx, agents)

	if !delivered["pA"] {
		t.Fatalf("pA should have been delivered to, got %v", delivered)
	}
	rest := withoutAgents(agents, delivered)
	if len(rest) != 1 || rest[0].AgentID != "pB" {
		t.Errorf("rest = %+v, want only pB — pA must sit out the remainder of the tick", rest)
	}
	// An empty exclusion set must not copy or reorder.
	if got := withoutAgents(agents, nil); len(got) != 2 {
		t.Errorf("withoutAgents(agents, nil) = %d agents, want 2", len(got))
	}
}
