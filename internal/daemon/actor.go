package daemon

import (
	"context"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// markCorrectionResolved flips the row a correction answers to "resolved",
// naming the correction's author as the row's actor (domain.AuditRecord.Actor).
// The daemon only EXECUTES this status change; the decision was the front
// end's that wrote the correction — the operator, or the orchestrator agent —
// so it is that author the Audit views must show, never "daemon".
func (d *Daemon) markCorrectionResolved(ctx context.Context, c domain.CorrectionRecord) error {
	if w, ok := d.opt.Store.(ports.AuditActorWriter); ok {
		return w.UpdateAuditStatusBy(ctx, c.AuditID, "resolved", c.Author)
	}
	return d.opt.Store.UpdateAuditStatus(ctx, c.AuditID, "resolved")
}
