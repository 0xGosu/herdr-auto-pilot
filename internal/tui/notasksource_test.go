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

// TestConfirmNoTaskSourceShowsANoteNotAnError: both confirm keys must render
// the guidance in the multi-line message area, never as a red one-line ✗ that
// truncates away the commands it names.
func TestConfirmNoTaskSourceShowsANoteNotAnError(t *testing.T) {
	// enter = confirm+send, y = confirm only. App.Confirm refuses before it
	// consults send, so both land on the notice.
	for _, key := range []string{"enter", "y"} {
		t.Run(key, func(t *testing.T) {
			m, _, _ := noticeModel(t)
			m, msg := pressResult(t, m, key)
			m = applyResult(t, m, msg)

			for _, want := range []string{
				"hap config set llm.task_generate_command --preset claude",
				"hap config set llm.task_generate_command --preset codex",
				"hap config task-source add",
				"hap dismiss",
			} {
				if !strings.Contains(m.message, want) {
					t.Errorf("message area is missing %q:\n%s", want, m.message)
				}
			}
			if m.status != nil && m.status.err {
				t.Errorf("a notice must not render as an error, got %q", m.status.text)
			}
		})
	}
}

// TestNoTaskSourceNoteSurvivesARefresh is the regression that would otherwise
// ship green and be invisible: the guidance lives in the message area, which
// must outlive the background refresh and clock ticks that run every second.
func TestNoTaskSourceNoteSurvivesARefresh(t *testing.T) {
	m, _, _ := noticeModel(t)
	m, msg := pressResult(t, m, "y")
	m = applyResult(t, m, msg)
	if m.message == "" {
		t.Fatal("no guidance was shown at all")
	}

	upd, _ := m.Update(tickMsg(time.Now()))
	m = upd.(Model)
	upd, _ = m.Update(refreshMsg{})
	m = upd.(Model)

	if !strings.Contains(m.message, "hap config set llm.task_generate_command") {
		t.Errorf("the guidance was wiped by a tick/refresh:\n%q", m.message)
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

	if !strings.Contains(m.message, "hap config set llm.task_generate_command") {
		t.Errorf("the batch dropped the notice guidance:\n%s", m.message)
	}
	if m.status == nil || m.status.err {
		t.Fatalf("the batch must still report its counts as a non-error note, got %+v", m.status)
	}
	if !strings.Contains(m.status.text, "confirmed 1") {
		t.Errorf("status = %q, want the count of what DID confirm", m.status.text)
	}
}
