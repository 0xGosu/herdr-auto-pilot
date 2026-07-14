package tui

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

// fakeHerdrTUI captures Send calls so the "also send?" yes-path can be
// asserted; ReadPane returns a standing menu for digit mapping.
type fakeHerdrTUI struct {
	inputs []string
	pane   string
}

func (f *fakeHerdrTUI) Send(_ context.Context, _, input string) error {
	f.inputs = append(f.inputs, input)
	return nil
}
func (f *fakeHerdrTUI) ReadPane(context.Context, string, int) (string, error) { return f.pane, nil }
func (f *fakeHerdrTUI) ListAgents(context.Context) ([]domain.AgentTransition, error) {
	return nil, nil
}

func correctTestModel(t *testing.T) (Model, *store.Store, *fakeHerdrTUI) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	fh := &fakeHerdrTUI{pane: "Do you want to proceed?\n❯ 1. Yes\n  2. No\n"}
	app := &frontend.App{
		Store:      st,
		Herdr:      fh,
		ConfigPath: filepath.Join(dir, "config.toml"),
		Author:     "operator",
	}
	return Model{width: 100, height: 30, app: app, ctx: context.Background()}, st, fh
}

func seedEscalation(t *testing.T, st *store.Store, status string) int64 {
	t.Helper()
	id, err := st.AppendAudit(context.Background(), domain.AuditRecord{
		AgentID: "w1:p1", Signature: "sig", Trigger: "t",
		SituationType: domain.SituationApproval, Action: "escalated",
		Status: status, Suggestion: "respond: y", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// runPromptSubmit fills the active prompt with input and presses Enter,
// returning the resulting model and the message its command produced.
func runPromptSubmit(t *testing.T, m Model, input string) (Model, tea.Msg) {
	t.Helper()
	if m.prompt == nil {
		t.Fatal("no active prompt")
	}
	m.prompt.input = input
	upd, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = upd.(Model)
	if cmd == nil {
		return m, nil
	}
	return m, cmd()
}

// TestCorrectLiveOpensSendPromptAndRecords: correcting a live escalation opens
// the "also send?" prompt; answering "n" records the correction as not sent.
func TestCorrectLiveRecordOnlyPath(t *testing.T) {
	m, st, fh := correctTestModel(t)
	id := seedEscalation(t, st, "escalated")

	upd, _ := m.correctByID(id, true)
	m = upd.(Model)
	if m.prompt == nil {
		t.Fatal("correct should open the action prompt")
	}

	// Submit the corrected action → chains to the send prompt.
	m, msg := runPromptSubmit(t, m, "Yes")
	sp, ok := msg.(openSendPromptMsg)
	if !ok {
		t.Fatalf("live correction should chain openSendPromptMsg, got %T", msg)
	}
	upd, _ = m.Update(sp)
	m = upd.(Model)
	if m.prompt == nil || m.prompt.input != "n" {
		t.Fatalf("send prompt should open defaulting to 'n', got %+v", m.prompt)
	}

	// Answer "n": record only, nothing sent.
	m, _ = runPromptSubmit(t, m, "n")
	corr, _ := st.UnprocessedCorrections(context.Background())
	if len(corr) != 1 || corr[0].Sent {
		t.Errorf("record-only correction should be Sent=false: %+v", corr)
	}
	if len(fh.inputs) != 0 {
		t.Errorf("answering 'n' must not deliver anything, got %v", fh.inputs)
	}
}

// TestCorrectLiveSendPath: answering "y" delivers the corrected action and
// records it as sent.
func TestCorrectLiveSendPath(t *testing.T) {
	m, st, fh := correctTestModel(t)
	id := seedEscalation(t, st, "escalated")

	upd, _ := m.correctByID(id, true)
	m = upd.(Model)
	m, msg := runPromptSubmit(t, m, "Yes")
	upd, _ = m.Update(msg.(openSendPromptMsg))
	m = upd.(Model)

	m, _ = runPromptSubmit(t, m, "y")
	corr, _ := st.UnprocessedCorrections(context.Background())
	if len(corr) != 1 || !corr[0].Sent {
		t.Errorf("sent correction should be Sent=true: %+v", corr)
	}
	if len(fh.inputs) != 1 {
		t.Errorf("answering 'y' should deliver exactly one keystroke, got %v", fh.inputs)
	}
}

// TestCorrectNonLiveRecordsWithoutSendPrompt: correcting a historical record
// (e.g. a past auto decision) records only and never opens the send prompt.
func TestCorrectNonLiveRecordsWithoutSendPrompt(t *testing.T) {
	m, st, fh := correctTestModel(t)
	id := seedEscalation(t, st, "auto") // not a pending escalation

	upd, _ := m.correctByID(id, false)
	m = upd.(Model)
	m, msg := runPromptSubmit(t, m, "n")
	if _, ok := msg.(openSendPromptMsg); ok {
		t.Fatal("non-live correction must NOT chain a send prompt")
	}
	corr, _ := st.UnprocessedCorrections(context.Background())
	if len(corr) != 1 || corr[0].Sent {
		t.Errorf("non-live correction should be Sent=false: %+v", corr)
	}
	if len(fh.inputs) != 0 {
		t.Errorf("non-live correction must not deliver, got %v", fh.inputs)
	}
}
