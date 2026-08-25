package tui

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/0xGosu/herdr-auto-pilot/internal/buildinfo"
	"github.com/0xGosu/herdr-auto-pilot/internal/daemonhealth"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

// fakeHerdrTUI captures Send calls so the "also send?" yes-path can be
// asserted; ReadPane returns a standing menu for digit mapping.
type fakeHerdrTUI struct {
	mu     sync.Mutex
	inputs []string
	pane   string
	agents []domain.AgentTransition // returned by ListAgents (live statuses)
}

// recordDelivered notes a reply the DAEMON would have typed.
//
// The TUI no longer delivers anything itself, so a test can no longer watch a
// pane to learn whether an operator's answer reached the agent. What it CAN
// watch is the queue: the stand-in drain below claims each queued delivery and
// records its action here, which keeps every "must / must not deliver"
// assertion meaning what it always did. What is typed for a given action —
// menu digit, answer series, remote-env keystrokes — is covered where it now
// happens, in internal/daemon's deliverreply tests.
func (f *fakeHerdrTUI) recordDelivered(action string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inputs = append(f.inputs, action)
}

func (f *fakeHerdrTUI) Send(_ context.Context, _, input string) error {
	f.recordDelivered(input)
	return nil
}

func (f *fakeHerdrTUI) delivered() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.inputs...)
}
func (f *fakeHerdrTUI) ReadPane(context.Context, string, int) (string, error) { return f.pane, nil }
func (f *fakeHerdrTUI) ListAgents(context.Context) ([]domain.AgentTransition, error) {
	return f.agents, nil
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
	// A health record that reads as a live daemon: the front end refuses to
	// queue an action nothing could execute, and these tests are all about
	// what an operator's keypress does when a daemon IS running.
	if err := daemonhealth.Write(dir, daemonhealth.Health{
		PID: os.Getpid(), HeartbeatAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	app := &frontend.App{
		Store:      st,
		Herdr:      fh,
		ConfigPath: filepath.Join(dir, "config.toml"),
		Author:     "operator",
		StateDir:   dir,
		DaemonInfo: func() (bool, int, string) { return true, os.Getpid(), buildinfo.Version },
	}
	startStandInDrain(t, st, fh.recordDelivered)
	return Model{width: 100, height: 30, app: app, ctx: context.Background()}, st, fh
}

// makeDaemonLive gives an App a health record that reads as a running daemon.
//
// The front end refuses to queue an operator action when nothing could execute
// it, so any test about what a keypress DOES has to stand a daemon up first.
func makeDaemonLive(t *testing.T, app *frontend.App, stateDir string) {
	t.Helper()
	if err := daemonhealth.Write(stateDir, daemonhealth.Health{
		PID: os.Getpid(), HeartbeatAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	app.StateDir = stateDir
	app.DaemonInfo = func() (bool, int, string) { return true, os.Getpid(), buildinfo.Version }
}

// startStandInDrain plays the daemon's agent-action drain: it claims each
// queued action, reports the reply it would have typed to record, and finishes
// the row so the operator's blocking wait returns.
func startStandInDrain(t *testing.T, st *store.Store, record func(string)) {
	t.Helper()
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		for {
			select {
			case <-done:
				return
			default:
			}
			acts, err := st.PendingAgentActions(context.Background())
			if err == nil {
				for _, a := range acts {
					ok, _ := st.ClaimAgentAction(context.Background(), a.ID, time.Now())
					if !ok {
						continue
					}
					var p domain.DeliverReplyPayload
					if json.Unmarshal([]byte(a.Payload), &p) == nil && p.Action != "" {
						record(p.Action)
					}
					if a.CorrectionID != 0 {
						st.MarkCorrectionSent(context.Background(), a.CorrectionID)
					}
					st.FinishAgentAction(context.Background(), a.ID,
						domain.AgentActionDone, "", "", time.Now())
				}
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
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
	if len(fh.delivered()) != 0 {
		t.Errorf("answering 'n' must not deliver anything, got %v", fh.delivered())
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

	runPromptSubmit(t, m, "y")
	corr, _ := st.UnprocessedCorrections(context.Background())
	if len(corr) != 1 || !corr[0].Sent {
		t.Errorf("sent correction should be Sent=true: %+v", corr)
	}
	if len(fh.delivered()) != 1 {
		t.Errorf("answering 'y' should deliver exactly one keystroke, got %v", fh.delivered())
	}
}

// TestCorrectNonLiveRecordsWithoutSendPrompt: correcting a historical record
// (e.g. a past auto decision) records only and never opens the send prompt.
func TestCorrectNonLiveRecordsWithoutSendPrompt(t *testing.T) {
	m, st, fh := correctTestModel(t)
	id := seedEscalation(t, st, "auto") // not a pending escalation

	upd, _ := m.correctByID(id, false)
	m = upd.(Model)
	_, msg := runPromptSubmit(t, m, "n")
	if _, ok := msg.(openSendPromptMsg); ok {
		t.Fatal("non-live correction must NOT chain a send prompt")
	}
	corr, _ := st.UnprocessedCorrections(context.Background())
	if len(corr) != 1 || corr[0].Sent {
		t.Errorf("non-live correction should be Sent=false: %+v", corr)
	}
	if len(fh.delivered()) != 0 {
		t.Errorf("non-live correction must not deliver, got %v", fh.delivered())
	}
}
