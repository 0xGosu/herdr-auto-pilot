package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// TestEscalationCarriesSignatureBaseline: escalate() already receives the
// SignatureResult, so persisting it is pure plumbing — but if it were dropped
// the auto-accept pass would find no baseline and silently never fire. Pin
// that a freshly raised escalation carries one, and that its salient window is
// concrete (never the 0 that would read as "unset").
func TestEscalationCarriesSignatureBaseline(t *testing.T) {
	h := newHarness(t, "")
	ctx := context.Background()
	h.herdr.setPane(approvalPane)
	h.herdr.setAgents([]domain.AgentTransition{
		{AgentID: "pB", PaneID: "pB", AgentType: "claude", Status: "blocked"},
	})

	h.daemon.reconcileAttention(ctx)

	var esc []domain.AuditRecord
	waitFor(t, 3*time.Second, func() bool {
		esc, _ = h.raw.PendingEscalations(ctx)
		return len(esc) == 1
	})
	if len(esc) != 1 {
		t.Fatalf("escalations = %d, want 1", len(esc))
	}
	got := esc[0]
	if got.SigRaw == "" {
		t.Error("SigRaw is empty: the escalation carries no baseline, so it could never auto-accept")
	}
	if got.SigSalient == "" {
		t.Error("SigSalient is empty: the jitter path would have nothing to compare")
	}
	if got.SigVerdict != domain.GuardOK {
		t.Errorf("SigVerdict = %q, want %q for a well-formed approval", got.SigVerdict, domain.GuardOK)
	}
	if got.SigSalientChars <= 0 {
		t.Errorf("SigSalientChars = %d, want the concrete window in effect", got.SigSalientChars)
	}
	// An approval's salient is STRUCTURED (verb + option set). This is what
	// keeps it on exact matching in SignatureHeldStill, so a baseline
	// accidentally rebuilt from the pane tail would compare quite differently.
	if !domain.StructuredSalient(got.SigSalient) {
		t.Errorf("an approval baseline must be a structured salient, got %q", got.SigSalient)
	}
	// The baseline must be the never-remapped content hash, which for a fresh
	// (unremapped) signature equals the learning key.
	if got.Signature != got.SigRaw {
		t.Errorf("Signature %q vs SigRaw %q: a fresh signature is not remapped", got.Signature, got.SigRaw)
	}
}

// TestAutonomousRowCarriesSignatureBaseline: a row written as "auto" can later
// be DEMOTED to escalated by Store.EscalateAudit, which has no signature in
// hand. Writing the baseline at insert time on auto rows too is what lets a
// demoted row carry one — and is why EscalateAudit itself needs no change.
func TestAutonomousRowCarriesSignatureBaseline(t *testing.T) {
	h := newHarness(t, "")
	ctx := context.Background()
	sig := h.seedAutonomous(approvalPane, domain.SituationApproval, "Yes")
	_ = sig

	h.herdr.setPane(approvalPane)
	h.push("pC", "blocked")

	var rows []domain.AuditRecord
	waitFor(t, 3*time.Second, func() bool {
		rows, _ = h.raw.AuditLog(ctx, 10)
		for _, r := range rows {
			if r.Status == "auto" && r.SigRaw != "" {
				return true
			}
		}
		return false
	})
	found := false
	for _, r := range rows {
		if r.Status == "auto" {
			found = true
			if r.SigRaw == "" {
				t.Errorf("auto row #%d carries no baseline; a later demotion would lose it", r.ID)
			}
			if r.SigSalientChars <= 0 {
				t.Errorf("auto row #%d has SigSalientChars=%d", r.ID, r.SigSalientChars)
			}
		}
	}
	if !found {
		t.Fatalf("no autonomous row was written; audit log: %+v", rows)
	}
}

// TestDemotedRowKeepsItsBaseline: Store.EscalateAudit rewrites only status,
// rationale and suggestion, so the baseline written at insert survives the
// demotion untouched. This is the invariant that lets escalateAudit stay
// unmodified.
func TestDemotedRowKeepsItsBaseline(t *testing.T) {
	h := newHarness(t, "")
	ctx := context.Background()

	id, err := h.raw.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "pD", AgentType: "claude", Trigger: "status",
		SituationType: domain.SituationChoice, Action: "auto:1 2 1",
		Status: "auto", CreatedAt: time.Now(),
	}.WithSignatureBaseline(domain.SignatureResult{
		Signature: "choice:abc", Raw: "choice:abc", Salient: "options:no;yes",
		Verdict: domain.GuardOK, SalientChars: 500,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.raw.EscalateAudit(ctx, id, "per-tab select kinds changed since capture", "answer series: 1 2 1"); err != nil {
		t.Fatal(err)
	}
	got, err := h.raw.GetAudit(ctx, id)
	if err != nil || got == nil {
		t.Fatalf("GetAudit: %+v %v", got, err)
	}
	if got.Status != "escalated" {
		t.Fatalf("Status = %q, want escalated", got.Status)
	}
	if got.SigRaw != "choice:abc" || got.SigSalient != "options:no;yes" || got.SigSalientChars != 500 {
		t.Errorf("demotion lost the baseline: raw=%q salient=%q chars=%d",
			got.SigRaw, got.SigSalient, got.SigSalientChars)
	}
}
