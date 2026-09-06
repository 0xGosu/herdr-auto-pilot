package tui

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/mattn/go-runewidth"
)

// snowflakeID is a real turso-engine id: store.TimeOrderedIDs packs 41 bits of
// milliseconds, 12 of node and 10 of sequence, which renders as 18 digits.
const snowflakeID int64 = 222206515109715968

func TestShortAuditIDKeepsTheTail(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   int64
		want string
	}{
		// The tail is what distinguishes a snowflake: the leading digits are a
		// timestamp every row of one session shares.
		{"snowflake", snowflakeID, "…15968"},
		{"another snowflake in the same millisecond range", 222205072571129856, "…29856"},
		// A sqlite AUTOINCREMENT rowid is short enough to print whole, and
		// printing it whole is what keeps every pre-turso database readable.
		{"sqlite rowid", 458, "#458"},
		{"rowid at the width limit", 12345, "#12345"},
		{"first rowid past the limit", 123456, "…23456"},
		{"one", 1, "#1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := shortAuditID(tc.id); got != tc.want {
				t.Errorf("shortAuditID(%d) = %q, want %q", tc.id, got, tc.want)
			}
		})
	}
}

// TestShortAuditIDNeverOverflowsTheIDColumn is the invariant the truncation
// exists for. The list formats reserve six columns for the id and then size
// their LAST column by subtracting a fixed prefix width (escPrefix, and the 87
// the Audit tab passes to budget). An id wider than its column shifts every
// later column out from under its header AND makes that subtraction wrong, so
// the rationale or action is clipped to a width the row no longer has.
func TestShortAuditIDNeverOverflowsTheIDColumn(t *testing.T) {
	const idColumnWidth = auditIDColWidth
	// Walk the whole range a row id can take, from the first sqlite rowid to
	// past the largest id 41 bits of milliseconds can mint.
	ids := []int64{1, 9, 10, 458, 12345, 99999, 100000, 123456,
		snowflakeID, 222202273271668741, 1<<62 - 1}
	for _, id := range ids {
		got := shortAuditID(id)
		if w := runewidth.StringWidth(got); w > idColumnWidth {
			t.Errorf("shortAuditID(%d) = %q renders %d columns wide, over the %d the ID column reserves",
				id, got, w, idColumnWidth)
		}
	}
}

// TestAuditRowNeverOverflowsTheContentWidth is the other half of the same
// invariant: bounding the ID column only helps if the row's FIXED prefix — the
// number renderAudit hands m.budget to size its last column — is the real one.
// It was written for an 8-wide STATUS while frontend.AuditStatusWidth is 11, so
// a row whose ACTION filled its column rendered two cells past contentWidth and
// wrapped, drawing more terminal lines than window()/listPageSize() budgeted
// and pushing the help line (and the last rows) off a full screen.
func TestAuditRowNeverOverflowsTheContentWidth(t *testing.T) {
	m := testModel(t)
	m.width, m.height = 120, 40
	upd, _ := m.Update(refreshMsg{status: m.data.status, audit: []domain.AuditRecord{{
		ID: snowflakeID, AgentID: "w6:p1", AgentType: "claude",
		SituationType: domain.SituationChoice, Status: "auto_accepted",
		// Long enough to fill whatever the last column was sized to.
		Action: strings.Repeat("x", 400), CreatedAt: time.Now(),
	}}})
	m = upd.(Model)
	m.tab = tabAudit
	var b strings.Builder
	m.renderAudit(&b)
	for _, ln := range strings.Split(strings.TrimRight(b.String(), "\n"), "\n") {
		if w := runewidth.StringWidth(ln); w > m.contentWidth() {
			t.Errorf("an audit row renders %d cells wide, over the %d contentWidth allows — it wraps:\n%s",
				w, m.contentWidth(), ln)
		}
	}
}

// TestShortAuditIDMarksThatItIsTruncated pins the ellipsis. Without it a
// truncated id is indistinguishable from a whole one, so an operator reads
// "#15968" off the screen and types it at `hap confirm` — which fails, or, if a
// short rowid from a pre-turso database happens to equal it, acts on the WRONG
// row. The marker is the only thing that says "there is more of this".
func TestShortAuditIDMarksThatItIsTruncated(t *testing.T) {
	short := shortAuditID(snowflakeID)
	if !strings.HasPrefix(short, "…") {
		t.Errorf("a truncated id must be marked with the ellipsis; got %q", short)
	}
	if strings.HasPrefix(short, "#") {
		t.Errorf("a truncated id must not wear the # that means a whole id; got %q", short)
	}
	whole := shortAuditID(458)
	if !strings.HasPrefix(whole, "#") || strings.Contains(whole, "…") {
		t.Errorf("an untruncated id keeps its # and gains no marker; got %q", whole)
	}
}

// TestListRowsTruncateTheIDButDetailShowsItWhole is the end-to-end half: the
// table shows the tail, and the detail an operator opens to act on the row
// still carries the id in full, which is what they need to type.
func TestListRowsTruncateTheIDButDetailShowsItWhole(t *testing.T) {
	m := testModel(t)
	now := time.Date(2026, 9, 6, 4, 0, 0, 0, time.UTC)
	upd, _ := m.Update(refreshMsg{
		status: m.data.status,
		escalations: []domain.AuditRecord{{
			ID: snowflakeID, AgentID: "w6:p1", AgentType: "claude",
			SituationType: domain.SituationApproval, Status: "escalated",
			Rationale: "low confidence", CreatedAt: now,
		}},
		audit: []domain.AuditRecord{{
			ID: snowflakeID, AgentID: "w6:p1", SituationType: domain.SituationChoice,
			Status: "auto", Action: "1", CreatedAt: now,
		}},
	})
	m = upd.(Model)

	full := fmt.Sprintf("#%d", snowflakeID)
	for _, tc := range []struct {
		name string
		tab  tab
	}{{"escalations", tabEscalations}, {"audit", tabAudit}} {
		t.Run(tc.name, func(t *testing.T) {
			m.tab = tc.tab
			m.cursors[m.tab] = 0
			view := m.View()
			if !strings.Contains(view, "…15968") {
				t.Errorf("the %s table should show the id's tail:\n%s", tc.name, view)
			}
			if strings.Contains(view, full) {
				t.Errorf("the %s table should not print the whole 18-digit id:\n%s", tc.name, view)
			}

			// The detail is where the full id has to survive.
			d := press(t, m, "v")
			detail := d.View()
			if !strings.Contains(detail, strconv.FormatInt(snowflakeID, 10)) {
				t.Errorf("the %s detail must carry the id in full:\n%s", tc.name, detail)
			}
		})
	}
}

// TestSearchStillFindsARowByTheDigitsOnScreen guards the pairing between the
// truncated display and the filter, which reads the FULL id. Because
// matchesQuery is a substring test, the visible tail still matches — but only
// as long as the filter keeps being given the whole id, so this fails if a
// later change "helpfully" narrows it to what is rendered.
func TestSearchStillFindsARowByTheDigitsOnScreen(t *testing.T) {
	m := testModel(t)
	now := time.Date(2026, 9, 6, 4, 0, 0, 0, time.UTC)
	upd, _ := m.Update(refreshMsg{
		status: m.data.status,
		audit: []domain.AuditRecord{
			{ID: snowflakeID, AgentID: "w6:p1", SituationType: domain.SituationChoice,
				Status: "auto", Action: "keep-me", CreatedAt: now},
			{ID: 222202273271668741, AgentID: "w6:p1", SituationType: domain.SituationChoice,
				Status: "auto", Action: "drop-me", CreatedAt: now},
		},
	})
	m = upd.(Model)
	m.tab = tabAudit

	// The tail an operator can actually read off the screen.
	m.query[tabAudit] = "15968"
	rows := m.filterAudit(tabAudit, m.data.audit)
	if len(rows) != 1 || rows[0].ID != snowflakeID {
		t.Fatalf("searching the visible tail should select exactly its row; got %v", rows)
	}
	// And the whole id still resolves, for anyone pasting from `hap audit`.
	m.query[tabAudit] = strconv.FormatInt(snowflakeID, 10)
	if rows = m.filterAudit(tabAudit, m.data.audit); len(rows) != 1 || rows[0].ID != snowflakeID {
		t.Fatalf("searching the whole id should still select its row; got %v", rows)
	}
}

// TestKillHistoryTruncatesItsIDToo — kill_events ids are minted by the same
// allocator, so the pause/resume table had the same overflow.
func TestKillHistoryTruncatesItsID(t *testing.T) {
	m := testModel(t)
	upd, _ := m.Update(refreshMsg{
		status: m.data.status,
		kills: []domain.KillEvent{{
			ID: snowflakeID, State: domain.KillStateActiveValue, Scope: "global", Author: "operator",
			CreatedAt: time.Date(2026, 9, 6, 4, 0, 0, 0, time.UTC),
		}},
	})
	m = upd.(Model)
	m.tab = tabKill
	view := m.View()
	if !strings.Contains(view, "…15968") {
		t.Errorf("the kill-history table should show the id's tail:\n%s", view)
	}
	if strings.Contains(view, fmt.Sprintf("#%d", snowflakeID)) {
		t.Errorf("the kill-history table should not print the whole id:\n%s", view)
	}
}
