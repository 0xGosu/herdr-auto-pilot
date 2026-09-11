package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// failingStore embeds the ports.StorePort INTERFACE, so an optional capability
// it does not forward is silently off for the whole suite. These forward
// ports.AuditActorWriter, without which every harness test would settle rows
// unattributed and TestCorrectionNamesItsAuthorOnTheSettledRow would be proving
// the fallback. The other StorePort-embedding doubles in this package do NOT
// forward it: a test asserting on AuditRecord.Actor must run over the harness
// (failingStore), or forward these five methods itself.

func (f *failingStore) ResolveEscalationBy(ctx context.Context, auditID int64, actor string) (bool, error) {
	if w, ok := f.StorePort.(ports.AuditActorWriter); ok {
		return w.ResolveEscalationBy(ctx, auditID, actor)
	}
	return f.ResolveEscalation(ctx, auditID)
}

func (f *failingStore) DismissEscalationBy(ctx context.Context, auditID int64, actor string) error {
	if w, ok := f.StorePort.(ports.AuditActorWriter); ok {
		return w.DismissEscalationBy(ctx, auditID, actor)
	}
	return f.DismissEscalation(ctx, auditID)
}

func (f *failingStore) UpdateAuditStatusBy(ctx context.Context, auditID int64, status, actor string) error {
	if w, ok := f.StorePort.(ports.AuditActorWriter); ok {
		return w.UpdateAuditStatusBy(ctx, auditID, status, actor)
	}
	return f.UpdateAuditStatus(ctx, auditID, status)
}

func (f *failingStore) DismissEscalationsBeforeBy(ctx context.Context, cutoff time.Time, actor string) (int64, error) {
	if w, ok := f.StorePort.(ports.AuditActorWriter); ok {
		return w.DismissEscalationsBeforeBy(ctx, cutoff, actor)
	}
	return f.DismissEscalationsBefore(ctx, cutoff)
}

func (f *failingStore) DismissEscalationsBeforeOnBy(ctx context.Context, cutoff time.Time, nodeID, actor string) (int64, error) {
	if w, ok := f.StorePort.(ports.AuditActorWriter); ok {
		return w.DismissEscalationsBeforeOnBy(ctx, cutoff, nodeID, actor)
	}
	return f.DismissEscalationsBeforeOn(ctx, cutoff, nodeID)
}

// TestCorrectionNamesItsAuthorOnTheSettledRow: the daemon EXECUTES the status
// change a correction implies, but the decision was the front end's, so both
// the answered escalation and the correction-lineage row must carry the
// correction's author — never the daemon, and never nobody.
func TestCorrectionNamesItsAuthorOnTheSettledRow(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setPane(approvalPane)
	ctx := context.Background()

	app := h.frontendApp()
	app.Author = domain.OrchestratorAuthor
	h.push("agent-actor", "blocked")
	var esc domain.AuditRecord
	waitFor(t, 3*time.Second, func() bool {
		pend, _ := h.raw.PendingEscalations(ctx)
		if len(pend) != 1 {
			return false
		}
		esc = pend[0]
		return true
	})
	if err := app.Resolve(ctx, esc.ID, "Yes", false); err != nil {
		t.Fatal(err)
	}

	var settled *domain.AuditRecord
	waitFor(t, 3*time.Second, func() bool {
		settled, _ = h.raw.GetAudit(ctx, esc.ID)
		return settled != nil && settled.Status == "resolved"
	})
	if settled.Actor != domain.OrchestratorAuthor {
		t.Errorf("the answered escalation is settled by %q, want %q", settled.Actor, domain.OrchestratorAuthor)
	}

	recs, err := h.raw.AuditLog(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	var lineage *domain.AuditRecord
	for i := range recs {
		if recs[i].CorrectsAuditID == esc.ID {
			lineage = &recs[i]
		}
	}
	if lineage == nil {
		t.Fatalf("no correction-lineage row for audit #%d in %+v", esc.ID, recs)
	}
	if lineage.Actor != domain.OrchestratorAuthor {
		t.Errorf("the correction-lineage row is by %q, want %q", lineage.Actor, domain.OrchestratorAuthor)
	}
}
