package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
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

// The end-to-end shape of #398, through the daemon's own sweep rather than the
// store call: a retired agent's wide row goes on the row-retention window, and
// the transition buffered before it vanished still finds nothing to resurrect.
//
// Excerpt retention is switched OFF on purpose. The two windows are independent
// settings and the roster half rides the row half, so this also pins that an
// operator who keeps every excerpt still gets a bounded agent_roster.
func TestRowRetentionPrunesARetiredAgentWithoutResurrectingIt(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)
	retiredAt := now.Add(-30 * 24 * time.Hour)
	agent := domain.RosterAgent{
		AgentID: "w1:p1", PaneID: "w1:p1", AgentType: "claude",
		Status: "idle", TerminalID: "term-a", SeenAt: retiredAt,
	}
	if err := st.PublishRoster(ctx, []domain.RosterAgent{agent}, retiredAt); err != nil {
		t.Fatal(err)
	}
	if err := st.PublishRoster(ctx, nil, retiredAt); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	excerptDays, rowDays := -1, 7
	cfg.Logging.AuditExcerptRetentionDays = &excerptDays
	cfg.Logging.RowRetentionDays = &rowDays
	d := &Daemon{
		cfg:         cfg,
		opt:         Options{Store: st},
		shutdownCtx: ctx,
		timers:      map[*time.Timer]struct{}{},
	}
	d.maybeRunRetentionSweep(now)
	d.bg.Wait()

	// Proof the SWEEP took the row, not just that the tombstone works: a direct
	// prune now finds nothing left to do. Without this the case passes on a
	// sweep that never touches agent_roster at all, since the tombstone alone
	// would still refuse the late event below.
	if c, err := st.PruneAgedRows(ctx, now, now.Add(-24*time.Hour)); err != nil || c.RetiredRoster != 0 {
		t.Fatalf("the daemon's sweep left %d retired roster row(s) behind (err=%v)", c.RetiredRoster, err)
	}

	late := agent
	late.Status = "working"
	late.SeenAt = now
	if err := st.UpsertRosterAgent(ctx, late); err != nil {
		t.Fatal(err)
	}
	roster, _, err := st.LiveRoster(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(roster) != 0 {
		t.Fatalf("late event resurrected a row row retention pruned: %+v", roster)
	}
}

// TestDedupConstantsMatchTheRetiredKeyWarnings pins a second relationship the
// type system cannot express, in the opposite direction from the one above.
//
// config.Load warns operators that the retired `limits.escalation_dedup_*` keys
// are now fixed internal constants, and it spells the VALUES out in prose
// because config cannot import daemon (daemon imports config). Nothing fails
// when they drift — config's own tests assert only on the KEY name — so the
// 5-minute text survived this window becoming 10 and told every operator with a
// legacy config something false. This test lives in daemon, which CAN see both
// sides, and fails the moment either constant stops matching its sentence.
func TestDedupConstantsMatchTheRetiredKeyWarnings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(
		"[limits]\nescalation_dedup_window_seconds = 300\nescalation_dedup_jitter_percent = 7\n"),
		0o600); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(prev)

	if _, err := config.Load(path); err != nil {
		t.Fatalf("load: %v", err)
	}

	got := logs.String()
	for _, want := range []string{
		fmt.Sprintf("fixed %d minutes", int(escalationDedupWindow/time.Minute)),
		fmt.Sprintf("fixed %d%%", escalationDedupJitterPercent),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the retired-key warning no longer states %q — a constant was retuned "+
				"without updating its operator-facing sentence in internal/config/config.go.\nwarnings: %s",
				want, got)
		}
	}
}
