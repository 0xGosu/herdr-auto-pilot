package daemon

import (
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

// TestEscalationDedupWindowFitsAuditExcerptMargin pins a relationship the type
// system cannot express: store cannot import daemon (daemon imports store), so
// store.PruneAuditExcerpts sets its own margin and this asserts the daemon's
// dedup window fits inside it.
//
// If the window ever grows past the margin, the sweep could blank the excerpt of
// a row PendingEscalationExcerpts is still comparing against, silently breaking
// duplicate-ask detection.
func TestEscalationDedupWindowFitsAuditExcerptMargin(t *testing.T) {
	if escalationDedupWindow > store.AuditExcerptDedupMargin {
		t.Fatalf("escalationDedupWindow (%v) exceeds store.AuditExcerptDedupMargin (%v): "+
			"the excerpt sweep could blank a row the dedup still reads",
			escalationDedupWindow, store.AuditExcerptDedupMargin)
	}
}

// TestRetentionIntervalIsDailyNotPerSweep guards the throttle: the sweep ticker
// fires every minute, and an unthrottled prune+VACUUM there would take a write
// lock 1,440 times a day.
func TestRetentionIntervalIsDailyNotPerSweep(t *testing.T) {
	if retentionInterval < 24*60*60*1e9 {
		t.Errorf("retentionInterval = %v, want at least 24h", retentionInterval)
	}
}
