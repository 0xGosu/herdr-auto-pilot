package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/control"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/verifyunblock"
)

// countDeliveryFailed returns how many audit rows the post-action self-check
// has written (Status == delivery_failed).
func (h *harness) countDeliveryFailed() int {
	h.t.Helper()
	rows, err := h.raw.AuditLog(context.Background(), 50)
	if err != nil {
		h.t.Fatalf("audit log: %v", err)
	}
	n := 0
	for _, r := range rows {
		if r.Status == verifyunblock.StatusFailed {
			n++
		}
	}
	return n
}

// TestSelfCheckAuditsStillBlocked: when an autonomous approval is delivered but
// the agent is STILL blocked a moment later, the daemon writes a
// delivery_failed audit row.
func TestSelfCheckAuditsStillBlocked(t *testing.T) {
	h := newHarness(t, "[limits]\nverify_unblock_ms = 20\n")
	h.herdr.setPane(approvalPane)
	h.seedAutonomous(approvalPane, domain.SituationApproval, "1")
	// The agent is reported as still blocked after the send.
	h.herdr.setAgents([]domain.AgentTransition{{AgentID: "agent-vb", PaneID: "agent-vb", Status: "blocked"}})

	h.push("agent-vb", "blocked")

	// The auto-send lands first.
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	// Then the self-check surfaces the still-blocked agent.
	waitFor(t, 3*time.Second, func() bool { return h.countDeliveryFailed() == 1 })

	rows, _ := h.raw.AuditLog(context.Background(), 50)
	var failed *domain.AuditRecord
	for i := range rows {
		if rows[i].Status == verifyunblock.StatusFailed {
			failed = &rows[i]
			break
		}
	}
	if failed == nil {
		t.Fatal("no delivery_failed audit row")
	}
	if failed.Input != "1" || failed.AgentID != "agent-vb" || failed.SituationType != domain.SituationApproval {
		t.Errorf("delivery_failed row fields mismatch: %+v", failed)
	}
}

// TestSelfCheckSilentWhenUnblocked: when the agent has left "blocked" by the
// time the self-check runs, no delivery_failed row is written.
func TestSelfCheckSilentWhenUnblocked(t *testing.T) {
	h := newHarness(t, "[limits]\nverify_unblock_ms = 20\n")
	h.herdr.setPane(approvalPane)
	h.seedAutonomous(approvalPane, domain.SituationApproval, "1")
	// The agent has moved on (no longer blocked).
	h.herdr.setAgents([]domain.AgentTransition{{AgentID: "agent-ok", PaneID: "agent-ok", Status: "working"}})

	h.push("agent-ok", "blocked")

	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	// Give the self-check (20ms) ample time to run, then assert it stayed quiet.
	time.Sleep(300 * time.Millisecond)
	if n := h.countDeliveryFailed(); n != 0 {
		t.Fatalf("want no delivery_failed rows for an unblocked agent, got %d", n)
	}
}

// TestSelfCheckDisabled: verify_unblock_ms = 0 disables the self-check even when
// the agent stays blocked.
func TestSelfCheckDisabled(t *testing.T) {
	h := newHarness(t, "[limits]\nverify_unblock_ms = 0\n")
	h.herdr.setPane(approvalPane)
	h.seedAutonomous(approvalPane, domain.SituationApproval, "1")
	h.herdr.setAgents([]domain.AgentTransition{{AgentID: "agent-off", PaneID: "agent-off", Status: "blocked"}})

	h.push("agent-off", "blocked")

	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	time.Sleep(300 * time.Millisecond)
	if n := h.countDeliveryFailed(); n != 0 {
		t.Fatalf("disabled self-check wrote %d rows", n)
	}
}

// seedEscalation inserts a pending approval escalation and returns its audit id,
// so an operator-correction self-check can be exercised.
func (h *harness) seedEscalationAudit(agentID string) int64 {
	h.t.Helper()
	id, err := h.raw.AppendAudit(context.Background(), domain.AuditRecord{
		AgentID: agentID, AgentType: "claude", Signature: "approval:opsig", Trigger: "t",
		SituationType: domain.SituationApproval, Action: "escalated", Status: "escalated",
		Suggestion: "respond: 1", PaneExcerpt: approvalPane, CreatedAt: time.Now(),
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return id
}

// TestSelfCheckOperatorSendStillBlocked: an operator correction DELIVERED to the
// agent (Sent=true) arms the self-check via the daemon's correction drain; a
// still-blocked agent gets a delivery_failed row.
func TestSelfCheckOperatorSendStillBlocked(t *testing.T) {
	h := newHarness(t, "[limits]\nverify_unblock_ms = 20\n")
	h.herdr.setAgents([]domain.AgentTransition{{AgentID: "agent-op", PaneID: "agent-op", Status: "blocked"}})
	ctx := context.Background()
	auditID := h.seedEscalationAudit("agent-op")

	if _, err := h.raw.InsertCorrection(ctx, domain.CorrectionRecord{
		AuditID: auditID, CorrectedAction: "1", Author: "operator", Sent: true, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := control.Nudge(ctx, h.ctlPath, control.KindReload); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 3*time.Second, func() bool { return h.countDeliveryFailed() == 1 })
}

// TestSelfCheckOperatorRecordOnly: a record-only correction (Sent=false) never
// arms the self-check, even against a still-blocked agent.
func TestSelfCheckOperatorRecordOnly(t *testing.T) {
	h := newHarness(t, "[limits]\nverify_unblock_ms = 20\n")
	h.herdr.setAgents([]domain.AgentTransition{{AgentID: "agent-ro", PaneID: "agent-ro", Status: "blocked"}})
	ctx := context.Background()
	auditID := h.seedEscalationAudit("agent-ro")

	if _, err := h.raw.InsertCorrection(ctx, domain.CorrectionRecord{
		AuditID: auditID, CorrectedAction: "1", Author: "operator", Sent: false, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := control.Nudge(ctx, h.ctlPath, control.KindReload); err != nil {
		t.Fatal(err)
	}

	// Wait for the correction lineage row (proves the drain ran), then assert
	// no delivery_failed row was written.
	waitFor(t, 3*time.Second, func() bool {
		log, _ := h.raw.AuditLog(ctx, 10)
		for _, r := range log {
			if r.CorrectsAuditID == auditID {
				return true
			}
		}
		return false
	})
	time.Sleep(200 * time.Millisecond)
	if n := h.countDeliveryFailed(); n != 0 {
		t.Fatalf("record-only correction must not arm the self-check, got %d rows", n)
	}
}

// TestSelfCheckSeriesStillBlocked: the multi-tab series delivery path also arms
// the self-check.
func TestSelfCheckSeriesStillBlocked(t *testing.T) {
	h := newHarness(t, "[limits]\nverify_unblock_ms = 20\n")
	h.herdr.setFrames(mcqFrames)
	h.seedSeriesRule(t, "1 2 1")
	h.herdr.setAgents([]domain.AgentTransition{{AgentID: "agent-mcq", PaneID: "agent-mcq", Status: "blocked"}})

	h.push("agent-mcq", "blocked")

	// The series delivery uses paced keystrokes; give it time, then the
	// self-check (armed after delivery) surfaces the still-blocked agent.
	waitFor(t, 10*time.Second, func() bool { return h.countDeliveryFailed() == 1 })
	// Sanity: the series went out as keystrokes, not text.
	if got := strings.Join(h.herdr.sentInputs(), " "); got != "" {
		t.Errorf("series must deliver as keystrokes, text sent: %q", got)
	}
}
