package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

// noticeModel parks a model on the Escalations tab with one [no_task_source]
// escalation carrying no suggestion — the notice raised when an idle agent has
// no task source and llm.task_generate_command is unset.
func noticeModel(t *testing.T) (Model, *store.Store, int64) {
	t.Helper()
	m, st, _ := correctTestModel(t)
	ctx := context.Background()
	id, err := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w1:p1", Signature: "sig", Trigger: "t",
		SituationType: domain.SituationIdle, Action: "escalated", Status: "escalated",
		Rationale: "[" + string(domain.ReasonNoTaskSource) + "]", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := st.GetAudit(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	m.tab = tabEscalations
	m.data.escalations = []domain.AuditRecord{*rec}
	m.cursors[tabEscalations] = 0
	return m, st, id
}

// pressResult sends one key and returns the model plus the action result its
// command produced, WITHOUT failing on an error — a notice arrives as one.
func pressResult(t *testing.T, m Model, key string) (Model, actionResultMsg) {
	t.Helper()
	upd, cmd := m.Update(pressKeyMsg(key))
	m = upd.(Model)
	if cmd == nil {
		t.Fatalf("%q produced no command", key)
	}
	msg, ok := cmd().(actionResultMsg)
	if !ok {
		t.Fatalf("%q produced %T, want actionResultMsg", key, cmd())
	}
	return m, msg
}

// applyResult feeds an action result back through Update, the way the runtime
// does when the command's goroutine finishes.
func applyResult(t *testing.T, m Model, msg actionResultMsg) Model {
	t.Helper()
	upd, _ := m.Update(msg)
	return upd.(Model)
}

// detailText joins the open overlay's lines for substring assertions.
func detailText(t *testing.T, m Model) string {
	t.Helper()
	if m.detail == nil {
		t.Fatal("no detail overlay is open")
	}
	return strings.Join(m.detail.lines, "\n")
}

// TestConfirmNoTaskSourceShowsANoteNotAnError: both confirm keys must open the
// guidance in the scrollable overlay, never as a red one-line ✗ that flattens
// away the commands it names.
func TestConfirmNoTaskSourceShowsANoteNotAnError(t *testing.T) {
	// enter = confirm+send, y = confirm only. App.Confirm refuses before it
	// consults send, so both land on the notice.
	for _, key := range []string{"enter", "y"} {
		t.Run(key, func(t *testing.T) {
			m, _, _ := noticeModel(t)
			m, msg := pressResult(t, m, key)
			m = applyResult(t, m, msg)

			got := detailText(t, m)
			for _, want := range []string{
				"hap config set llm.task_generate_command --preset claude",
				"hap config set llm.task_generate_command --preset codex",
				"hap config task-source add",
				"hap dismiss",
			} {
				if !strings.Contains(got, want) {
					t.Errorf("the overlay is missing %q:\n%s", want, got)
				}
			}
			// Not an error, and not a bogus success either: a lone notice
			// confirmed nothing, so a green "confirmed 0" would be a check
			// mark on a no-op. The overlay is the whole answer here.
			if m.status != nil {
				t.Errorf("a lone notice must leave no status note, got %+v", m.status)
			}
			// Nothing on this overlay is answerable — that is the message —
			// so it must not offer the escalation actions.
			if m.detail.confirmID != 0 || m.detail.retryID != 0 {
				t.Errorf("the notice overlay must offer no confirm/retry, got %+v", m.detail)
			}
		})
	}
}

// TestNoTaskSourceGuidanceFitsAShortPane is the regression this overlay exists
// for. The guidance is a dozen lines; rendered into the ephemeral message area
// — budgeted as a flat two rows by chromeRows — it pushed the help line and
// the rows the operator was reading off a short terminal. The overlay scrolls,
// so it fits at any size.
func TestNoTaskSourceGuidanceFitsAShortPane(t *testing.T) {
	for _, size := range []struct{ w, h int }{{40, 12}, {80, 30}} {
		m, _, _ := noticeModel(t)
		m.width, m.height = size.w, size.h
		m, msg := pressResult(t, m, "y")
		m = applyResult(t, m, msg)

		view := m.View()
		if rows := strings.Count(view, "\n") + 1; rows > m.height {
			t.Errorf("%dx%d: the guidance renders %d rows in a %d-row pane:\n%s",
				size.w, size.h, rows, m.height, view)
		}
		// Whatever does not fit must say it can be scrolled to, never be
		// silently dropped — the commands ARE the message.
		if !strings.Contains(view, "hap config set llm.task_generate_command") &&
			!strings.Contains(view, "to scroll") {
			t.Errorf("%dx%d: the guidance is neither shown nor scrollable:\n%s",
				size.w, size.h, view)
		}
	}
}

// TestNoTaskSourceGuidanceIsReadableOnAnOrdinaryTerminal: the clipping above is
// only acceptable because a normal pane shows the commands without scrolling.
func TestNoTaskSourceGuidanceIsReadableOnAnOrdinaryTerminal(t *testing.T) {
	m, _, _ := noticeModel(t)
	m.width, m.height = 100, 40
	m, msg := pressResult(t, m, "y")
	m = applyResult(t, m, msg)

	view := m.View()
	for _, want := range []string{
		"hap config set llm.task_generate_command --preset claude",
		"hap config task-source add",
		"hap dismiss",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("a 100x40 pane must show %q without scrolling:\n%s", want, view)
		}
	}
}

// TestConfirmNoTaskSourceBatchReportsBothHalves: over a marked batch the count
// summary and the guidance are different information, so a notice must not be
// folded into an error string that swallows one of them.
func TestConfirmNoTaskSourceBatchReportsBothHalves(t *testing.T) {
	m, st, noticeID := noticeModel(t)
	ctx := context.Background()
	// A second, ordinarily confirmable escalation so the batch has both shapes.
	okID, err := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w1:p1", Signature: "sig2", Trigger: "t",
		SituationType: domain.SituationApproval, Action: "escalated",
		Status: "escalated", Suggestion: "respond: 2", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	okRec, err := st.GetAudit(ctx, okID)
	if err != nil {
		t.Fatal(err)
	}
	m.data.escalations = append(m.data.escalations, *okRec)
	m.marked = map[int64]bool{noticeID: true, okID: true}

	m, msg := pressResult(t, m, "y")
	m = applyResult(t, m, msg)

	if got := detailText(t, m); !strings.Contains(got, "hap config set llm.task_generate_command") {
		t.Errorf("the batch dropped the notice guidance:\n%s", got)
	}
	if m.status == nil || m.status.err {
		t.Fatalf("the batch must still report its counts as a non-error note, got %+v", m.status)
	}
	if !strings.Contains(m.status.text, "confirmed 1") {
		t.Errorf("status = %q, want the count of what DID confirm", m.status.text)
	}
}
