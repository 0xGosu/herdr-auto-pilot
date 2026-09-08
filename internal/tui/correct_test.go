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
// liveAgents reads the fake's statuses under its lock — the stand-in drain runs
// on its own goroutine while a test mutates them.
func (f *fakeHerdrTUI) liveAgents() []domain.AgentTransition {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.AgentTransition(nil), f.agents...)
}

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
		ConfigPath: seedLocalFSConfigIn(t, dir),
		Author:     "operator",
		StateDir:   dir,
		DaemonInfo: func() (bool, int, string) { return true, os.Getpid(), buildinfo.Version },
	}
	startStandInDrainWithConfirm(t, st, fh.recordDelivered, app, fh)
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
	startStandInDrainWithConfirm(t, st, record, nil, nil)
}

// startStandInDrainWithAgents is the stand-in that also answers the one
// question the real accept_generated_task executor asks herdr: is the agent
// still parked?
//
// A confirm is a QUEUED action now, so the refusal for a busy agent is authored
// on the owning node and travels back as text. A drain that answered "done" for
// every kind would report a task delivered to an agent that is mid-conversation
// — and the TUI's "add to the list instead?" offer, which keys off that
// refusal, would never appear in any test.
func startStandInDrainWithConfirm(t *testing.T, st *store.Store, record func(string),
	app *frontend.App, fh *fakeHerdrTUI) {
	t.Helper()
	done := make(chan struct{})
	// The cleanup must WAIT for the goroutine, not merely signal it. Cleanups
	// run LIFO, so signalling alone lets this pass keep querying while
	// st.Close() and t.TempDir()'s RemoveAll are already running behind it —
	// and a SQLite query recreates the -wal and -shm files the moment after
	// RemoveAll deletes them, failing the test with "directory not empty".
	// The test that reports it is then whichever one happened to be cheap
	// enough to reach cleanup while the goroutine was still mid-query, which
	// is why it reads as an unrelated flake.
	stopped := make(chan struct{})
	t.Cleanup(func() {
		close(done)
		<-stopped
	})
	go func() {
		defer close(stopped)
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
					if a.Kind == domain.AgentActionAcceptGeneratedTask && app != nil {
						standInConfirm(st, a, app, fh)
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

// standInConfirm plays the accept_generated_task executor: the send-time
// staleness gate (with the shared marker the front end matches to restore the
// sentinel), then the REAL confirm.
//
// Calling the real one matters. These tests assert what a keypress causes — the
// tasks file written, the source registered, the correction recorded, the task
// delivered — and all of that now happens on the owning node. A drain that only
// answered "done" would leave every one of those assertions checking nothing.
func standInConfirm(st *store.Store, a domain.AgentAction, app *frontend.App, fh *fakeHerdrTUI) {
	ctx := context.Background()
	var p domain.AcceptGeneratedTaskPayload
	_ = json.Unmarshal([]byte(a.Payload), &p)
	if p.Send {
		for _, ag := range fh.liveAgents() {
			if ag.AgentID == a.Target && domain.AgentBusy(ag.Status) {
				st.FinishAgentAction(ctx, a.ID, domain.AgentActionFailed,
					"agent is no longer idle; "+domain.SuggestionStaleMarker+
						" (agent status: "+ag.Status+") — dismiss it, or confirm without --send "+
						"to queue the tasks to the agent's list", "", time.Now())
				return
			}
		}
	}
	if err := app.ConfirmGeneratedTaskForOperator(ctx, p.AuditID, p.Send, a.Author, standInHost{fh}); err != nil {
		st.FinishAgentAction(ctx, a.ID, domain.AgentActionFailed, err.Error(), "", time.Now())
		return
	}
	st.FinishAgentAction(ctx, a.ID, domain.AgentActionDone, "", "", time.Now())
}

// standInHost is the pane access the daemon supplies in production
// (ports.TaskSendHost).
type standInHost struct{ fh *fakeHerdrTUI }

func (h standInHost) Cwd(context.Context, string) string { return "" }

func (h standInHost) Send(_ context.Context, _, _, prompt string) error {
	h.fh.recordDelivered(prompt)
	return nil
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
	msg := cmd()
	// An action dispatched from inside a prompt claims its rows first — a
	// prompt's onSubmit returns a tea.Cmd and cannot mutate the model — and
	// carries the real command as the claim's payload. Following that hop is
	// exactly what the Bubble Tea runtime does, and it is what keeps every
	// assertion below pointed at what the ACTION did rather than at the claim.
	if bs, ok := msg.(beginSendingMsg); ok {
		upd, _ := m.Update(bs)
		m = upd.(Model)
		if bs.run == nil {
			return m, nil
		}
		msg = bs.run()
	}
	return m, msg
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
