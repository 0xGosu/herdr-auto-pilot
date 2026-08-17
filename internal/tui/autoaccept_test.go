package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// auditModelWith builds a Model whose Audit tab shows rows.
func auditModelWith(t *testing.T, rows []domain.AuditRecord) Model {
	t.Helper()
	m := Model{width: 180, height: 40}
	msg := refreshMsg{cfg: config.Default()}
	msg.audit = rows
	upd, _ := m.Update(msg)
	m = upd.(Model)
	m.tab = tabAudit
	return m
}

// TestAuditViewRendersAutoAcceptStatuses: neither new status may render as
// unknown, and an auto-accept must be visually separable from an operator's
// own resolution — the machine acted, and nothing was learned from it.
func TestAuditViewRendersAutoAcceptStatuses(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	m := auditModelWith(t, []domain.AuditRecord{
		{ID: 1, SituationType: domain.SituationApproval, Status: domain.AuditStatusAutoAccepted,
			Action: "auto-accepted:1", CreatedAt: now},
		{ID: 2, SituationType: domain.SituationApproval, Status: "resolved",
			Action: "corrected:1", CreatedAt: now},
		{ID: 3, SituationType: domain.SituationApproval, Status: domain.AuditStatusAutoAccepting,
			Action: "escalated", CreatedAt: now},
	})
	view := m.View()

	for _, want := range []string{"auto-sent", "resolved", "sending"} {
		if !strings.Contains(view, want) {
			t.Errorf("audit view missing %q:\n%s", want, view)
		}
	}
	// The raw status values must never leak into the column — they are wider
	// than it and would shift ACTION.
	for _, raw := range []string{domain.AuditStatusAutoAccepted, domain.AuditStatusAutoAccepting} {
		if strings.Contains(view, raw) {
			t.Errorf("raw status %q rendered instead of its label:\n%s", raw, view)
		}
	}
}

// TestAuditViewSurfacesMachineDismissalReasonsInline: `dismissed` now has two
// authors and three machine reasons. The reason lives in the rationale, which
// the list has no room for, so it must reach the row itself — an operator must
// not have to open each record to tell them apart.
func TestAuditViewSurfacesMachineDismissalReasonsInline(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	m := auditModelWith(t, []domain.AuditRecord{
		{ID: 1, SituationType: domain.SituationApproval, Status: "dismissed",
			Rationale: "[shadow_mode] learning [auto_dismiss_stale] drift", Action: "escalated", CreatedAt: now},
		{ID: 2, SituationType: domain.SituationApproval, Status: "dismissed",
			Rationale: "[shadow_mode] learning [auto_dismiss_agent_gone]", Action: "escalated", CreatedAt: now},
		{ID: 3, SituationType: domain.SituationApproval, Status: "dismissed",
			Rationale: "[shadow_mode] learning [auto_accept_failed] 3 attempts", Action: "escalated", CreatedAt: now},
		{ID: 4, SituationType: domain.SituationApproval, Status: "dismissed",
			Rationale: "[shadow_mode] learning", Action: "escalated", CreatedAt: now},
	})
	view := m.View()

	for _, want := range []string{"dism:stale", "dism:gone", "dism:failed", "dismissed"} {
		if !strings.Contains(view, want) {
			t.Errorf("audit view missing %q:\n%s", want, view)
		}
	}
}

// TestAuditColumnsSurviveTheWidestStatus: the STATUS column must be wide enough
// for every label it can now produce, or a machine dismissal silently shifts
// the ACTION column beside it.
func TestAuditColumnsSurviveTheWidestStatus(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	m := auditModelWith(t, []domain.AuditRecord{
		{ID: 1, SituationType: domain.SituationApproval, Status: "auto",
			Action: "MARKER-SHORT", CreatedAt: now},
		{ID: 2, SituationType: domain.SituationApproval, Status: "dismissed",
			Rationale: "[shadow_mode] x [auto_accept_failed] y", Action: "MARKER-WIDE", CreatedAt: now},
	})
	lines := strings.Split(m.View(), "\n")
	col := func(marker string) int {
		for _, l := range lines {
			if i := strings.Index(l, marker); i >= 0 {
				return i
			}
		}
		t.Fatalf("marker %q not rendered:\n%s", marker, m.View())
		return -1
	}
	if a, b := col("MARKER-SHORT"), col("MARKER-WIDE"); a != b {
		t.Errorf("the ACTION column shifted from %d to %d under the widest status label", a, b)
	}
}

// TestEscalationsListSurvivesATransientRefresh covers the pending-queue flicker:
// a failed delivery moves a row escalated -> auto_accepting -> escalated within
// one tick, so it briefly leaves and re-enters the list. The view must absorb
// that without the operator taking any action to recover it.
func TestEscalationsListSurvivesATransientRefresh(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	rows := []domain.AuditRecord{
		{ID: 10, SituationType: domain.SituationApproval, Status: "escalated", Rationale: "[shadow_mode] alpha", CreatedAt: now},
		{ID: 11, SituationType: domain.SituationApproval, Status: "escalated", Rationale: "[shadow_mode] bravo", CreatedAt: now},
		{ID: 12, SituationType: domain.SituationApproval, Status: "escalated", Rationale: "[shadow_mode] charlie", CreatedAt: now},
	}
	m := Model{width: 180, height: 40}
	msg := refreshMsg{cfg: config.Default()}
	msg.escalations = rows
	upd, _ := m.Update(msg)
	m = upd.(Model)
	m.tab = tabEscalations
	m.query[tabEscalations] = "shadow_mode" // an active filter matching every row
	m.cursors[tabEscalations] = 2

	// Mid-tick: #11 is claimed, so it drops out of the pending set.
	msg2 := refreshMsg{cfg: config.Default()}
	msg2.escalations = []domain.AuditRecord{rows[0], rows[2]}
	upd, _ = m.Update(msg2)
	m = upd.(Model)
	if got := m.cursors[tabEscalations]; got >= 2 {
		// Clamping to the shorter list is the correct behavior; what must NOT
		// happen is an out-of-range cursor that panics or blanks the view.
		t.Logf("cursor clamped to %d on the shorter list", got)
	}
	if view := m.View(); view == "" {
		t.Fatal("the view went blank while a row was transiently claimed")
	}

	// The delivery failed, so the row is reverted and reappears.
	msg3 := refreshMsg{cfg: config.Default()}
	msg3.escalations = rows
	upd, _ = m.Update(msg3)
	m = upd.(Model)

	if got := m.query[tabEscalations]; got != "shadow_mode" {
		t.Errorf("the active filter was lost across the transient: %q", got)
	}
	if got := m.cursors[tabEscalations]; got < 0 || got >= len(rows) {
		t.Errorf("cursor = %d, out of range for %d rows", got, len(rows))
	}
	view := m.View()
	for _, want := range []string{"#10", "#11", "#12"} {
		if !strings.Contains(view, want) {
			t.Errorf("row %s did not come back after the transient:\n%s", want, view)
		}
	}
}

// TestAuditRowStyleMarksFullSelfPromptingRows: "auto-sent" is the status for
// BOTH flavours of automatic acceptance, so the operator's only at-a-glance
// signal for "the mode I switched on did this" is the row colour.
//
// Asserted on the style CHOICE rather than the rendered escape sequence:
// lipgloss drops colour when tests run without a TTY, so a Contains check on
// the ANSI would pass whatever the renderer decided.
func TestAuditRowStyleMarksFullSelfPromptingRows(t *testing.T) {
	st := defaultStyles
	fspRow := domain.AuditRecord{
		ID: 1, Status: domain.AuditStatusAutoAccepted, WhileFSPModeOn: true,
	}
	timedRow := domain.AuditRecord{
		ID: 2, Status: domain.AuditStatusAutoAccepted,
	}

	got, ok := auditRowStyle(st, fspRow, false)
	if !ok {
		t.Fatal("a full self-prompting row must be styled, not left plain")
	}
	if got.GetForeground() != st.warn.GetForeground() {
		t.Errorf("foreground = %v, want the warn role %v", got.GetForeground(), st.warn.GetForeground())
	}

	if _, ok := auditRowStyle(st, timedRow, false); ok {
		t.Error("a timed auto-accept must stay plain, or the colour means nothing")
	}

	// Selected wins, or the cursor vanishes on exactly the rows worth opening.
	sel, ok := auditRowStyle(st, fspRow, true)
	if !ok || sel.GetForeground() != st.selected.GetForeground() {
		t.Errorf("selected row = %v, want the selected role %v", sel.GetForeground(), st.selected.GetForeground())
	}
}

// TestAuditDetailNamesFullSelfPromptingAsTheCause: the colour needs a caption
// somewhere, or an operator has to guess what amber means.
func TestAuditDetailNamesFullSelfPromptingAsTheCause(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	m := auditModelWith(t, nil)
	rec := domain.AuditRecord{
		ID: 1, SituationType: domain.SituationApproval, Status: domain.AuditStatusAutoAccepted,
		Action: "Yes", WhileFSPModeOn: true, CreatedAt: now,
	}
	detail := strings.Join(m.auditDetailLines(rec, "", 120, auditDetailOptions{}), "\n")
	if !strings.Contains(detail, "full self-prompting") {
		t.Errorf("audit detail does not name full self-prompting as the cause:\n%s", detail)
	}

	rec.WhileFSPModeOn = false
	plain := strings.Join(m.auditDetailLines(rec, "", 120, auditDetailOptions{}), "\n")
	if strings.Contains(plain, "Caused by") {
		t.Errorf("an ordinary row must not carry a cause line:\n%s", plain)
	}
}
