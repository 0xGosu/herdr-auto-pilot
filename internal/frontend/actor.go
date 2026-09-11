package frontend

// Attribution of settled escalations (domain.AuditRecord.Actor): which front
// end resolved or dismissed a row — the operator, or the orchestrator agent —
// so the Audit views can tell the two apart.

import (
	"context"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// AuditActorWidth is the TUI Audit tab's BY column width, sized to the
// longest label AuditActorLabel returns.
const AuditActorWidth = 4

// AuditActorLabel renders who settled an audit row, short enough for a table
// column: "op" for the operator, "orch" for the orchestrator agent, "-" when
// nobody is named (the daemon's own rows, and rows predating the column).
// Any other stored value is cut to the column rather than dropped, so a name
// written by a newer build still shows something.
func AuditActorLabel(r domain.AuditRecord) string {
	switch r.Actor {
	case "":
		return "-"
	case domain.OperatorAuthor:
		return "op"
	case domain.OrchestratorAuthor:
		return "orch"
	}
	if len(r.Actor) > AuditActorWidth {
		return r.Actor[:AuditActorWidth]
	}
	return r.Actor
}

// The store writes below name the actor when the store can record one
// (ports.AuditActorWriter) and fall back to the unattributed write otherwise:
// the name is display-only, so a store without the column must still settle
// the row.

func (a *App) resolveEscalationBy(ctx context.Context, auditID int64, actor string) (bool, error) {
	if w, ok := a.Store.(ports.AuditActorWriter); ok {
		return w.ResolveEscalationBy(ctx, auditID, actor)
	}
	return a.Store.ResolveEscalation(ctx, auditID)
}

func (a *App) dismissEscalationBy(ctx context.Context, auditID int64, actor string) error {
	if w, ok := a.Store.(ports.AuditActorWriter); ok {
		return w.DismissEscalationBy(ctx, auditID, actor)
	}
	return a.Store.DismissEscalation(ctx, auditID)
}

func (a *App) dismissEscalationsBeforeBy(ctx context.Context, cutoff time.Time, actor string) (int64, error) {
	if w, ok := a.Store.(ports.AuditActorWriter); ok {
		return w.DismissEscalationsBeforeBy(ctx, cutoff, actor)
	}
	return a.Store.DismissEscalationsBefore(ctx, cutoff)
}

func (a *App) dismissEscalationsBeforeOnBy(ctx context.Context, cutoff time.Time, nodeID, actor string) (int64, error) {
	if w, ok := a.Store.(ports.AuditActorWriter); ok {
		return w.DismissEscalationsBeforeOnBy(ctx, cutoff, nodeID, actor)
	}
	return a.Store.DismissEscalationsBeforeOn(ctx, cutoff, nodeID)
}
