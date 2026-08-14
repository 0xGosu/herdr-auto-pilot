package tui

import (
	"bytes"
	"context"
	"sync"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

func bellModel() (Model, *bytes.Buffer) {
	var buf bytes.Buffer
	return Model{width: 100, height: 30, bellOut: &buf, inflight: &sync.WaitGroup{}}, &buf
}

func TestBellNoRingOnFirstRefresh(t *testing.T) {
	m, buf := bellModel()
	upd, _ := m.Update(refreshMsg{
		cfg:         config.Config{TUI: config.TUI{TerminalBell: true}},
		status:      frontend.Status{Paused: true},
		escalations: []domain.AuditRecord{{ID: 5}},
	})
	m = upd.(Model)
	if buf.Len() != 0 {
		t.Fatalf("first refresh must never ring, even with pre-existing escalations/pause; got %v", buf.Bytes())
	}
	if !m.initialized {
		t.Fatal("initialized should be true after the first successful refresh")
	}
}

func TestBellRingsOnNewEscalation(t *testing.T) {
	m, buf := bellModel()
	cfg := config.Config{TUI: config.TUI{TerminalBell: true}}

	upd, _ := m.Update(refreshMsg{cfg: cfg, escalations: []domain.AuditRecord{{ID: 1}}})
	m = upd.(Model)
	if buf.Len() != 0 {
		t.Fatalf("baseline refresh must not ring, got %v", buf.Bytes())
	}

	m.Update(refreshMsg{cfg: cfg, escalations: []domain.AuditRecord{{ID: 1}, {ID: 2}}})
	if got := buf.Bytes(); len(got) != 1 || got[0] != 0x07 {
		t.Fatalf("new escalation should ring exactly one BEL, got %v", got)
	}
}

func TestBellNoRingWithoutNewEscalation(t *testing.T) {
	m, buf := bellModel()
	cfg := config.Config{TUI: config.TUI{TerminalBell: true}}
	rows := []domain.AuditRecord{{ID: 1}}

	upd, _ := m.Update(refreshMsg{cfg: cfg, escalations: rows})
	m = upd.(Model)
	m.Update(refreshMsg{cfg: cfg, escalations: rows})
	if buf.Len() != 0 {
		t.Fatalf("unchanged escalations must not ring, got %v", buf.Bytes())
	}
}

func TestBellToggleOffSuppressesEscalationRing(t *testing.T) {
	m, buf := bellModel()
	cfg := config.Config{TUI: config.TUI{TerminalBell: false}}

	upd, _ := m.Update(refreshMsg{cfg: cfg, escalations: []domain.AuditRecord{{ID: 1}}})
	m = upd.(Model)
	m.Update(refreshMsg{cfg: cfg, escalations: []domain.AuditRecord{{ID: 1}, {ID: 2}}})
	if buf.Len() != 0 {
		t.Fatalf("toggle off must suppress the bell, got %v", buf.Bytes())
	}
}

func TestBellRingsOnExternallyCausedPause(t *testing.T) {
	m, buf := bellModel()
	cfg := config.Config{TUI: config.TUI{TerminalBell: true}}

	upd, _ := m.Update(refreshMsg{cfg: cfg, status: frontend.Status{Paused: false}})
	m = upd.(Model)
	m.Update(refreshMsg{cfg: cfg, status: frontend.Status{Paused: true}})
	if got := buf.Bytes(); len(got) != 1 || got[0] != 0x07 {
		t.Fatalf("externally-caused pause should ring exactly one BEL, got %v", got)
	}
}

func TestBellNoRingOnSelfCausedPause(t *testing.T) {
	m, buf := bellModel()
	cfg := config.Config{TUI: config.TUI{TerminalBell: true}}

	upd, _ := m.Update(refreshMsg{cfg: cfg, status: frontend.Status{Paused: false}})
	m = upd.(Model)
	m.pausePending = true // simulates the "p" key handler having just fired
	upd, _ = m.Update(refreshMsg{cfg: cfg, status: frontend.Status{Paused: true}})
	m = upd.(Model)
	if buf.Len() != 0 {
		t.Fatalf("self-caused pause must not ring, got %v", buf.Bytes())
	}
	if m.pausePending {
		t.Fatal("pausePending should be consumed once the matching transition is observed")
	}
}

// TestBellSelfPauseRaceRefreshBeforeActionResult pins down the exact
// ordering Bubble Tea can produce: the periodic poll's refreshMsg (which
// already reflects the new pause via a fast local DB read) can be delivered
// to Update() before this instance's own actionResultMsg from pauseCmd (a
// slower round trip). Because pausePending is set synchronously in the "p"
// key handler — before pauseCmd is even dispatched — it must already be
// true by the time ANY later message is processed, regardless of which
// goroutine's result arrives first. This is the race the pause-vs-tick
// ordering review flagged; this test proves it can't happen with the
// synchronous-flag design.
func TestBellSelfPauseRaceRefreshBeforeActionResult(t *testing.T) {
	m, buf := bellModel()
	cfg := config.Config{TUI: config.TUI{TerminalBell: true}}

	upd, _ := m.Update(refreshMsg{cfg: cfg, status: frontend.Status{Paused: false}})
	m = upd.(Model)

	// Simulates the "p" keypress's own Update call: it sets pausePending
	// synchronously and returns a command, but we deliberately do NOT feed
	// that command's resulting actionResultMsg yet.
	m.pausePending = true

	// The 2s poll's refreshMsg "wins the race" and is processed first.
	upd, _ = m.Update(refreshMsg{cfg: cfg, status: frontend.Status{Paused: true}})
	m = upd.(Model)
	if buf.Len() != 0 {
		t.Fatalf("a self-caused pause must not ring even if its refreshMsg arrives before its actionResultMsg; got %v", buf.Bytes())
	}

	// The actionResultMsg finally arrives; it must not double-count or panic.
	upd, _ = m.Update(actionResultMsg{message: "automation paused", pauseAction: true})
	m = upd.(Model)
	if buf.Len() != 0 {
		t.Fatalf("the delayed actionResultMsg must not itself ring, got %v", buf.Bytes())
	}
}

func TestBellPausePendingClearedOnFailedPauseAction(t *testing.T) {
	m, buf := bellModel()
	cfg := config.Config{TUI: config.TUI{TerminalBell: true}}

	upd, _ := m.Update(refreshMsg{cfg: cfg, status: frontend.Status{Paused: false}})
	m = upd.(Model)
	m.pausePending = true

	// The pause attempt failed: Paused never transitions, so nothing would
	// otherwise consume the flag.
	upd, _ = m.Update(actionResultMsg{err: errBoom, pauseAction: true})
	m = upd.(Model)
	if m.pausePending {
		t.Fatal("a failed pause action must clear pausePending, not leave it dangling")
	}

	// A later, genuinely external pause must now correctly ring.
	upd, _ = m.Update(refreshMsg{cfg: cfg, status: frontend.Status{Paused: true}})
	m = upd.(Model)
	if got := buf.Bytes(); len(got) != 1 || got[0] != 0x07 {
		t.Fatalf("external pause after a cleared pausePending should ring, got %v", got)
	}
}

// A "p" press while already paused is a no-op: like a failed pause, Paused
// never transitions, so the no-op result itself must consume the flag.
func TestBellPausePendingClearedOnNoOpPauseAction(t *testing.T) {
	m, buf := bellModel()
	cfg := config.Config{TUI: config.TUI{TerminalBell: true}}

	// Already paused when "p" is pressed (an earlier refresh said so).
	upd, _ := m.Update(refreshMsg{cfg: cfg, status: frontend.Status{Paused: true}})
	m = upd.(Model)
	m.pausePending = true

	upd, _ = m.Update(actionResultMsg{message: "automation already paused", pauseAction: true, pauseNoChange: true})
	m = upd.(Model)
	if m.pausePending {
		t.Fatal("a no-op pause action must clear pausePending, not leave it dangling")
	}

	// A later external resume→pause cycle must now correctly ring.
	upd, _ = m.Update(refreshMsg{cfg: cfg, status: frontend.Status{Paused: false}})
	m = upd.(Model)
	upd, _ = m.Update(refreshMsg{cfg: cfg, status: frontend.Status{Paused: true}})
	m = upd.(Model)
	if got := buf.Bytes(); len(got) != 1 || got[0] != 0x07 {
		t.Fatalf("external pause after a cleared pausePending should ring, got %v", got)
	}
}

// A rapid DOUBLE-press of "p" from an unpaused state: the second press's
// no-op result lands before the refresh that reports the first press's
// false→true transition. The no-op must NOT clear pausePending there —
// the pause is self-caused, and clearing would ring the bell for it.
func TestBellNoOpPauseBeforeObservedTransitionKeepsSuppression(t *testing.T) {
	m, buf := bellModel()
	cfg := config.Config{TUI: config.TUI{TerminalBell: true}}

	upd, _ := m.Update(refreshMsg{cfg: cfg, status: frontend.Status{Paused: false}})
	m = upd.(Model)
	m.pausePending = true // both presses set it synchronously

	// First press's result, then the second press's no-op result — both
	// arrive before the next refresh.
	upd, _ = m.Update(actionResultMsg{message: "automation paused", pauseAction: true})
	m = upd.(Model)
	upd, _ = m.Update(actionResultMsg{message: "automation already paused", pauseAction: true, pauseNoChange: true})
	m = upd.(Model)
	if !m.pausePending {
		t.Fatal("a no-op result before the transition is observed must keep pausePending")
	}

	// The refresh finally reports the operator's own pause: no bell.
	upd, _ = m.Update(refreshMsg{cfg: cfg, status: frontend.Status{Paused: true}})
	m = upd.(Model)
	if got := buf.Bytes(); len(got) != 0 {
		t.Fatalf("self-caused pause should not ring after a no-op second press, got %v", got)
	}
	if m.pausePending {
		t.Fatal("the observed transition should consume pausePending")
	}
}

func TestBellNilOutputNeverPanics(t *testing.T) {
	m := Model{width: 100, height: 30} // bellOut left nil
	cfg := config.Config{TUI: config.TUI{TerminalBell: true}}

	upd, _ := m.Update(refreshMsg{cfg: cfg, status: frontend.Status{Paused: false}})
	m = upd.(Model)
	upd, _ = m.Update(refreshMsg{cfg: cfg, status: frontend.Status{Paused: true}})
	_ = upd.(Model) // must not panic
}

// TestPauseKeyPressSetsPausePendingSynchronously drives a real "p" keypress
// through a Model wired to a real store/App (matching this file's other
// action tests) and asserts pausePending is already true on the returned
// Model — i.e. before the pauseCmd's result is ever fed back in.
func TestPauseKeyPressSetsPausePendingSynchronously(t *testing.T) {
	m, _, _ := appModel(t)

	upd, cmd := m.Update(pressKeyMsg("p"))
	m = upd.(Model)
	if cmd == nil {
		t.Fatal("p should return the pause command")
	}
	if !m.pausePending {
		t.Fatal("pausePending should be set synchronously by the \"p\" key handler")
	}

	msg, ok := cmd().(actionResultMsg)
	if !ok || msg.err != nil || !msg.pauseAction {
		t.Fatalf("pauseCmd should report a successful pauseAction result, got %+v", msg)
	}
}

type boomError struct{}

func (boomError) Error() string { return "boom" }

var errBoom = boomError{}

// fakeNotifier records the toasts the TUI raises and replays a canned herdr
// answer, standing in for the socket adapter.
type fakeNotifier struct {
	mu    sync.Mutex
	calls []struct{ title, body string }
	res   ports.NotifyResult
	err   error
}

func (f *fakeNotifier) ShowNotification(_ context.Context, title, body string) (ports.NotifyResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, struct{ title, body string }{title, body})
	return f.res, f.err
}

func (f *fakeNotifier) Calls() []struct{ title, body string } {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]struct{ title, body string }(nil), f.calls...)
}

// notifyModel builds a model wired to a fake notifier. shown/err are the
// canned answer; ctx is real because alert() passes m.ctx to the notifier.
func notifyModel(shown bool, err error) (Model, *bytes.Buffer, *fakeNotifier) {
	var buf bytes.Buffer
	f := &fakeNotifier{res: ports.NotifyResult{Shown: shown, Reason: "shown"}, err: err}
	if !shown {
		f.res.Reason = "rate_limited"
	}
	m := Model{
		width: 100, height: 30,
		bellOut:  &buf,
		inflight: &sync.WaitGroup{},
		ctx:      context.Background(),
		notifier: f,
	}
	return m, &buf, f
}

// bothOn enables the toast and keeps the bell as its fallback.
func bothOn() config.Config {
	return config.Config{TUI: config.TUI{TerminalBell: true, HerdrNotification: true}}
}

// settle waits for alert()'s goroutine, which delivers the toast off the
// update loop so a socket round trip cannot freeze the UI.
func settle(m Model) { m.inflight.Wait() }

func TestNotifyShownSuppressesBell(t *testing.T) {
	m, buf, f := notifyModel(true, nil)
	cfg := bothOn()

	upd, _ := m.Update(refreshMsg{cfg: cfg, escalations: []domain.AuditRecord{{ID: 1}}})
	m = upd.(Model)
	upd, _ = m.Update(refreshMsg{cfg: cfg, escalations: []domain.AuditRecord{
		{ID: 1},
		{ID: 2, AgentID: "w1:p3", SituationType: domain.SituationApproval, Suggestion: "Yes"},
	}})
	m = upd.(Model)
	settle(m)

	calls := f.Calls()
	if len(calls) != 1 {
		t.Fatalf("want exactly 1 notification, got %d", len(calls))
	}
	if calls[0].title != "Auto Prompter: w1:p3 needs attention" {
		t.Errorf("title = %q", calls[0].title)
	}
	if calls[0].body != "approval escalated. Suggestion: Yes" {
		t.Errorf("body = %q", calls[0].body)
	}
	if buf.Len() != 0 {
		t.Fatalf("a delivered toast must not also ring the bell, got %v", buf.Bytes())
	}
}

// TestNotifyNotShownFallsBackToBell is the core of the feature: herdr saying
// "I dropped it" (disabled, rate limited, no foreground client, busy) must
// still reach the operator.
func TestNotifyNotShownFallsBackToBell(t *testing.T) {
	m, buf, f := notifyModel(false, nil)
	cfg := bothOn()

	upd, _ := m.Update(refreshMsg{cfg: cfg, escalations: []domain.AuditRecord{{ID: 1}}})
	m = upd.(Model)
	upd, _ = m.Update(refreshMsg{cfg: cfg, escalations: []domain.AuditRecord{{ID: 1}, {ID: 2}}})
	m = upd.(Model)
	settle(m)

	if len(f.Calls()) != 1 {
		t.Fatalf("want 1 notification attempt, got %d", len(f.Calls()))
	}
	if got := buf.Bytes(); len(got) != 1 || got[0] != 0x07 {
		t.Fatalf("an undelivered toast should ring exactly one BEL, got %v", got)
	}
}

func TestNotifyErrorFallsBackToBell(t *testing.T) {
	m, buf, _ := notifyModel(true, errBoom)
	cfg := bothOn()

	upd, _ := m.Update(refreshMsg{cfg: cfg, escalations: []domain.AuditRecord{{ID: 1}}})
	m = upd.(Model)
	upd, _ = m.Update(refreshMsg{cfg: cfg, escalations: []domain.AuditRecord{{ID: 1}, {ID: 2}}})
	m = upd.(Model)
	settle(m)

	if got := buf.Bytes(); len(got) != 1 || got[0] != 0x07 {
		t.Fatalf("a failed notification should ring exactly one BEL, got %v", got)
	}
}

// TestNotifyWithBellOffStaysSilent: the bell is only ever a fallback for an
// operator who asked for it. Turning it off must not resurrect it.
func TestNotifyWithBellOffStaysSilent(t *testing.T) {
	m, buf, f := notifyModel(false, nil)
	cfg := config.Config{TUI: config.TUI{TerminalBell: false, HerdrNotification: true}}

	upd, _ := m.Update(refreshMsg{cfg: cfg, escalations: []domain.AuditRecord{{ID: 1}}})
	m = upd.(Model)
	upd, _ = m.Update(refreshMsg{cfg: cfg, escalations: []domain.AuditRecord{{ID: 1}, {ID: 2}}})
	m = upd.(Model)
	settle(m)

	if len(f.Calls()) != 1 {
		t.Fatalf("the toast should still be attempted, got %d calls", len(f.Calls()))
	}
	if buf.Len() != 0 {
		t.Fatalf("bell is off; nothing should be written, got %v", buf.Bytes())
	}
}

// TestNotifyToggleOffNeverCallsNotifier: with herdr_notification off the
// behavior is exactly what it was before the feature — bell only, socket
// untouched.
func TestNotifyToggleOffNeverCallsNotifier(t *testing.T) {
	m, buf, f := notifyModel(true, nil)
	cfg := config.Config{TUI: config.TUI{TerminalBell: true, HerdrNotification: false}}

	upd, _ := m.Update(refreshMsg{cfg: cfg, escalations: []domain.AuditRecord{{ID: 1}}})
	m = upd.(Model)
	upd, _ = m.Update(refreshMsg{cfg: cfg, escalations: []domain.AuditRecord{{ID: 1}, {ID: 2}}})
	m = upd.(Model)
	settle(m)

	if len(f.Calls()) != 0 {
		t.Fatalf("notifier must not be called when the option is off, got %d", len(f.Calls()))
	}
	if got := buf.Bytes(); len(got) != 1 || got[0] != 0x07 {
		t.Fatalf("bell should still ring, got %v", got)
	}
}

// TestNewPropagatesNotifierFromApp guards the single line that connects the
// wiring to the behavior. Every other test here injects the notifier into a
// Model literal, so dropping `notifier: app.Notifier` from New would leave the
// whole feature dead in production with a fully green suite.
func TestNewPropagatesNotifierFromApp(t *testing.T) {
	f := &fakeNotifier{res: ports.NotifyResult{Shown: true}}
	m := New(context.Background(), &frontend.App{Notifier: f})
	if m.notifier == nil {
		t.Fatal("New must carry App.Notifier onto the model")
	}

	// And a plain terminal (no herdr, so App.Notifier is nil) must leave it nil
	// rather than a non-nil interface holding a nil pointer, which would make
	// alert()'s `m.notifier != nil` check pass and then panic.
	if plain := New(context.Background(), &frontend.App{}); plain.notifier != nil {
		t.Errorf("notifier should be nil without an App.Notifier, got %#v", plain.notifier)
	}
}

// TestPausePendingConsumedWhileAlertsOff: pausePending records who caused a
// pause, so it must be cleared by the transition itself. Leaving it set
// because alerts happened to be off would latch it, and the NEXT pause — an
// external one, with alerts back on — would read as self-caused and be
// silently swallowed.
func TestPausePendingConsumedWhileAlertsOff(t *testing.T) {
	m, buf, f := notifyModel(true, nil)
	off := config.Config{TUI: config.TUI{TerminalBell: false, HerdrNotification: false}}

	upd, _ := m.Update(refreshMsg{cfg: off, status: frontend.Status{Paused: false}})
	m = upd.(Model)
	m.pausePending = true // this instance pressed "p"
	upd, _ = m.Update(refreshMsg{cfg: off, status: frontend.Status{Paused: true}})
	m = upd.(Model)
	settle(m)
	if m.pausePending {
		t.Fatal("pausePending must be consumed by the transition even with alerts off")
	}

	// Unpause, turn alerting back on, and take an EXTERNAL pause: it must alert.
	upd, _ = m.Update(refreshMsg{cfg: bothOn(), status: frontend.Status{Paused: false}})
	m = upd.(Model)
	upd, _ = m.Update(refreshMsg{cfg: bothOn(), status: frontend.Status{Paused: true}})
	m = upd.(Model)
	settle(m)

	if len(f.Calls()) != 1 {
		t.Fatalf("the later external pause must alert, got %d calls", len(f.Calls()))
	}
	if buf.Len() != 0 {
		t.Errorf("the toast was shown; the bell should stay quiet, got %v", buf.Bytes())
	}
}

// TestNotifyBothOffIsFullySilent covers the fourth corner of the matrix; the
// trigger block must not even evaluate when neither channel is enabled.
func TestNotifyBothOffIsFullySilent(t *testing.T) {
	m, buf, f := notifyModel(true, nil)
	cfg := config.Config{TUI: config.TUI{TerminalBell: false, HerdrNotification: false}}

	upd, _ := m.Update(refreshMsg{cfg: cfg, escalations: []domain.AuditRecord{{ID: 1}}})
	m = upd.(Model)
	upd, _ = m.Update(refreshMsg{cfg: cfg,
		escalations: []domain.AuditRecord{{ID: 1}, {ID: 2}},
		status:      frontend.Status{Paused: true}})
	m = upd.(Model)
	settle(m)

	if len(f.Calls()) != 0 || buf.Len() != 0 {
		t.Fatalf("both channels off must be silent; %d calls, %v bytes", len(f.Calls()), buf.Bytes())
	}
}

func TestNotifyNoneOnFirstRefresh(t *testing.T) {
	m, _, f := notifyModel(true, nil)

	upd, _ := m.Update(refreshMsg{
		cfg:         bothOn(),
		status:      frontend.Status{Paused: true},
		escalations: []domain.AuditRecord{{ID: 5}},
	})
	m = upd.(Model)
	settle(m)

	if len(f.Calls()) != 0 {
		t.Fatalf("the first refresh must never notify, got %d", len(f.Calls()))
	}
}

func TestNotifyOnExternallyCausedPause(t *testing.T) {
	m, _, f := notifyModel(true, nil)
	cfg := bothOn()

	upd, _ := m.Update(refreshMsg{cfg: cfg, status: frontend.Status{Paused: false}})
	m = upd.(Model)
	upd, _ = m.Update(refreshMsg{cfg: cfg, status: frontend.Status{Paused: true}})
	m = upd.(Model)
	settle(m)

	calls := f.Calls()
	if len(calls) != 1 {
		t.Fatalf("want 1 notification, got %d", len(calls))
	}
	if calls[0].title != "Auto Prompter: automation paused" {
		t.Errorf("title = %q", calls[0].title)
	}
}

func TestNotifyNotOnSelfCausedPause(t *testing.T) {
	m, buf, f := notifyModel(true, nil)
	cfg := bothOn()

	upd, _ := m.Update(refreshMsg{cfg: cfg, status: frontend.Status{Paused: false}})
	m = upd.(Model)
	m.pausePending = true
	upd, _ = m.Update(refreshMsg{cfg: cfg, status: frontend.Status{Paused: true}})
	m = upd.(Model)
	settle(m)

	if len(f.Calls()) != 0 || buf.Len() != 0 {
		t.Fatalf("a self-caused pause must alert through neither channel")
	}
}

// TestNotifyNilNotifierFallsBackToBell is the non-herdr case: hap launched
// from a plain terminal has no socket, so App.Notifier is nil.
func TestNotifyNilNotifierFallsBackToBell(t *testing.T) {
	m, buf := bellModel()
	cfg := bothOn()

	upd, _ := m.Update(refreshMsg{cfg: cfg, escalations: []domain.AuditRecord{{ID: 1}}})
	m = upd.(Model)
	m.Update(refreshMsg{cfg: cfg, escalations: []domain.AuditRecord{{ID: 1}, {ID: 2}}})

	if got := buf.Bytes(); len(got) != 1 || got[0] != 0x07 {
		t.Fatalf("without a notifier the bell must still ring, got %v", got)
	}
}

// TestNotifyNilNotifierAndNilOutputNeverPanics guards the fully-unwired model
// the way TestBellNilOutputNeverPanics does for the bell alone.
func TestNotifyNilNotifierAndNilOutputNeverPanics(t *testing.T) {
	m := Model{width: 100, height: 30, inflight: &sync.WaitGroup{}} // no bellOut, no notifier
	cfg := bothOn()

	upd, _ := m.Update(refreshMsg{cfg: cfg, status: frontend.Status{Paused: false}})
	m = upd.(Model)
	upd, _ = m.Update(refreshMsg{cfg: cfg, status: frontend.Status{Paused: true}})
	m = upd.(Model)
	settle(m)
}

// TestNotifyOneAlertPerPollForABurst: several escalations landing in one poll
// is still one toast — the same rule the bell always followed.
func TestNotifyOneAlertPerPollForABurst(t *testing.T) {
	m, _, f := notifyModel(true, nil)
	cfg := bothOn()

	upd, _ := m.Update(refreshMsg{cfg: cfg, escalations: []domain.AuditRecord{{ID: 1}}})
	m = upd.(Model)
	upd, _ = m.Update(refreshMsg{cfg: cfg, escalations: []domain.AuditRecord{
		{ID: 1}, {ID: 2, AgentID: "a"}, {ID: 3, AgentID: "b"}, {ID: 4, AgentID: "c"},
	}})
	m = upd.(Model)
	settle(m)

	calls := f.Calls()
	if len(calls) != 1 {
		t.Fatalf("a burst should raise exactly 1 toast, got %d", len(calls))
	}
	// The newest row is the one described.
	if calls[0].title != "Auto Prompter: c needs attention" {
		t.Errorf("the highest-id escalation should be named, got %q", calls[0].title)
	}
}

// TestNotifyUsesAgentDisplayName: the toast and the Agents tab must name the
// same agent, so it resolves the short name herdr/hap knows it by.
func TestNotifyUsesAgentDisplayName(t *testing.T) {
	m, _, f := notifyModel(true, nil)
	cfg := bothOn()
	status := frontend.Status{AgentNames: map[string]string{"w1:p3": "backend"}}

	upd, _ := m.Update(refreshMsg{cfg: cfg, status: status,
		escalations: []domain.AuditRecord{{ID: 1}}})
	m = upd.(Model)
	upd, _ = m.Update(refreshMsg{cfg: cfg, status: status, escalations: []domain.AuditRecord{
		{ID: 1}, {ID: 2, AgentID: "w1:p3", SituationType: domain.SituationError},
	}})
	m = upd.(Model)
	settle(m)

	calls := f.Calls()
	if len(calls) != 1 || calls[0].title != "Auto Prompter: backend needs attention" {
		t.Fatalf("want the agent's short name in the title, got %+v", calls)
	}
	if calls[0].body != "error escalated." {
		t.Errorf("body = %q", calls[0].body)
	}
}

// TestNotifyWithNoEscalationRow: a max-id bump with no matching row (a pruned
// or filtered list) must still alert, just without specifics — never panic on
// an empty record and never send an empty title, which herdr rejects.
func TestNotifyWithNoEscalationRow(t *testing.T) {
	m, _, f := notifyModel(true, nil)
	cfg := bothOn()

	upd, _ := m.Update(refreshMsg{cfg: cfg, escalations: []domain.AuditRecord{{ID: 1}}})
	m = upd.(Model)
	// ID 2 with no AgentID at all — the degenerate shape.
	upd, _ = m.Update(refreshMsg{cfg: cfg, escalations: []domain.AuditRecord{{ID: 2}}})
	m = upd.(Model)
	settle(m)

	calls := f.Calls()
	if len(calls) != 1 {
		t.Fatalf("want 1 notification, got %d", len(calls))
	}
	if calls[0].title == "" {
		t.Error("title must never be empty — herdr rejects it with invalid_params")
	}
}

// TestNotifyFailedRefreshDoesNotAlert: a failed poll carries a zero config
// and empty escalations; it must not look like "everything resolved" and then
// re-alert on the next good poll.
func TestNotifyFailedRefreshDoesNotAlert(t *testing.T) {
	m, buf, f := notifyModel(true, nil)
	cfg := bothOn()

	upd, _ := m.Update(refreshMsg{cfg: cfg, escalations: []domain.AuditRecord{{ID: 7}}})
	m = upd.(Model)
	upd, _ = m.Update(refreshMsg{err: errBoom})
	m = upd.(Model)
	settle(m)
	if len(f.Calls()) != 0 || buf.Len() != 0 {
		t.Fatalf("a failed refresh must not alert")
	}

	// The baseline survived the failure, so the unchanged set is still quiet.
	upd, _ = m.Update(refreshMsg{cfg: cfg, escalations: []domain.AuditRecord{{ID: 7}}})
	m = upd.(Model)
	settle(m)
	if len(f.Calls()) != 0 || buf.Len() != 0 {
		t.Fatalf("the baseline must survive a failed refresh; %d calls", len(f.Calls()))
	}
}
