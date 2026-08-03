package tui

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

// refreshWith builds a refresh carrying one agent in the given status, plus an
// optional newest-escalation id — enough for the fingerprint to move.
func refreshWith(status string, escID int64) refreshMsg {
	msg := refreshMsg{
		status: frontend.Status{
			MonitoredAgents: []domain.AgentTransition{
				{AgentID: "w1:p1", AgentType: "claude", Status: status},
			},
		},
	}
	if escID > 0 {
		msg.escalations = []domain.AuditRecord{{ID: escID, AgentID: "w1:p1", Status: "escalated"}}
	}
	return msg
}

// TestIdlePollBacksOffOnlyAfterRealQuiet pins the cadence policy. Each poll is
// a full store read plus two herdr CLI round trips, so a pane left open
// overnight is thousands of queries answering "still nothing" — but backing off
// while anything is happening would make the operator's console lag.
func TestIdlePollBacksOffOnlyAfterRealQuiet(t *testing.T) {
	base := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)

	t.Run("a fresh model polls fast", func(t *testing.T) {
		var m Model // lastActivity zero: nothing observed yet
		if m.idle(base) {
			t.Error("a model that has never refreshed must not be considered idle")
		}
		if got := m.refreshInterval(base); got != fastPollInterval {
			t.Errorf("interval = %v, want %v", got, fastPollInterval)
		}
	})

	t.Run("just under the threshold stays fast", func(t *testing.T) {
		m := Model{lastActivity: base}
		at := base.Add(idleBackoffAfter - time.Second)
		if m.idle(at) {
			t.Error("backed off before the quiet period elapsed")
		}
		if got := m.refreshInterval(at); got != fastPollInterval {
			t.Errorf("interval = %v, want %v", got, fastPollInterval)
		}
		if got := m.clockInterval(at); got != fastClockInterval {
			t.Errorf("clock = %v, want %v", got, fastClockInterval)
		}
	})

	t.Run("at and past the threshold backs off", func(t *testing.T) {
		m := Model{lastActivity: base}
		for _, at := range []time.Time{
			base.Add(idleBackoffAfter),
			base.Add(4 * idleBackoffAfter),
		} {
			if !m.idle(at) {
				t.Errorf("%v after the last activity is not idle", at.Sub(base))
			}
			if got := m.refreshInterval(at); got != slowPollInterval {
				t.Errorf("interval = %v, want %v", got, slowPollInterval)
			}
			if got := m.clockInterval(at); got != slowClockInterval {
				t.Errorf("clock = %v, want %v", got, slowClockInterval)
			}
		}
	})
}

// TestChangedDataCountsAsActivity is what keeps an unattended TUI live while
// agents are working: nobody touches the keyboard for hours, so only the data
// itself can say the herd is busy.
func TestChangedDataCountsAsActivity(t *testing.T) {
	var m Model
	mm, _ := m.Update(refreshWith("idle", 0))
	m = mm.(Model)
	first := m.lastActivity
	if first.IsZero() {
		t.Fatal("the first refresh did not stamp activity")
	}

	// Same data again: nothing happened, so the stamp must NOT move — this is
	// the whole mechanism, since a stamp that always moves never backs off.
	m.lastActivity = first.Add(-idleBackoffAfter)
	stale := m.lastActivity
	mm, _ = m.Update(refreshWith("idle", 0))
	m = mm.(Model)
	if !m.lastActivity.Equal(stale) {
		t.Error("an unchanged refresh moved the activity stamp; the poll would never back off")
	}

	// An agent transition is an event even with no operator present.
	mm, _ = m.Update(refreshWith("working", 0))
	m = mm.(Model)
	if !m.lastActivity.After(stale) {
		t.Error("an agent status change did not count as activity")
	}

	// So is a new escalation.
	m.lastActivity = stale
	mm, _ = m.Update(refreshWith("working", 77))
	m = mm.(Model)
	if !m.lastActivity.After(stale) {
		t.Error("a new escalation did not count as activity")
	}
}

// TestFailedRefreshDoesNotLookLikeQuiet: a store that started erroring returns
// early with no data, which must not read as "nothing is happening" and back
// the poll off exactly when the operator needs it most.
func TestFailedRefreshDoesNotLookLikeQuiet(t *testing.T) {
	m := Model{lastActivity: time.Now().Add(-idleBackoffAfter)}
	stale := m.lastActivity
	failed := refreshWith("idle", 0)
	failed.err = errors.New("store unavailable")
	mm, _ := m.Update(failed)
	m = mm.(Model)
	if !m.lastActivity.After(stale) {
		t.Error("a failed refresh was treated as quiet")
	}
}

// TestKeypressRestoresTheFastPoll covers the return path, including the trap
// that a keypress must not start a SECOND ticker beside the slow one already in
// flight — two tickers would double every refresh for the rest of the session,
// quietly costing more than the back-off saves.
//
// The ticker assertion is made possible by bubbletea collapsing a Batch with a
// single non-nil member down to that member: with a key whose handleKey returns
// no command, a correct implementation hands back the refresh command ITSELF,
// while any extra scheduled command shows up as a tea.BatchMsg. Asserting
// merely that cmd != nil would pass either way, which is the whole point.
func TestKeypressRestoresTheFastPoll(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	app := &frontend.App{Store: st, ConfigPath: filepath.Join(dir, "config.toml"), Author: "op"}

	m := New(ctx, app)
	m.width, m.height = 100, 30
	m.lastActivity = time.Now().Add(-2 * idleBackoffAfter)

	// A tick while idle schedules the slow cadence and records that it did.
	mm, _ := m.Update(tickMsg(time.Now()))
	m = mm.(Model)
	if !m.slowPoll {
		t.Fatal("an idle tick did not record that it scheduled the slow poll")
	}

	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m = mm.(Model)
	if m.slowPoll {
		t.Error("a keypress left the model on the slow poll")
	}
	if m.idle(time.Now()) {
		t.Error("a keypress did not reset the idle clock")
	}
	if got := m.refreshInterval(time.Now()); got != fastPollInterval {
		t.Errorf("interval after a keypress = %v, want %v", got, fastPollInterval)
	}
	if m.now.IsZero() {
		t.Error("the render clock was not advanced, so Age would stay stale after the operator returns")
	}
	if cmd == nil {
		t.Fatal("returning from the slow poll must pull fresh data at once")
	}
	switch got := cmd().(type) {
	case refreshMsg:
		// Exactly one command, and it is the refresh: correct.
	case tea.BatchMsg:
		t.Fatalf("the keypress scheduled %d commands; it must refresh WITHOUT starting a "+
			"second ticker beside the slow one still in flight", len(got))
	default:
		t.Fatalf("keypress command produced %T, want refreshMsg", got)
	}

	// A keypress while ALREADY fast must not force a refresh at all: that path
	// would run a full store read on every keystroke.
	m.slowPoll = false
	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}}); cmd != nil {
		t.Errorf("a keypress on the fast poll produced %T; it must not refresh per keystroke", cmd())
	}
}

// TestTicksScheduleExactlyOneSuccessor is the liveness half of the duplication
// trap: each timer message must schedule one successor — never zero (the TUI
// would stop updating) and never two.
func TestTicksScheduleExactlyOneSuccessor(t *testing.T) {
	m := testModel(t)
	m.data.updateDue = false // keep the update check out of the batch

	// The data tick legitimately batches its refresh with its next tick, so
	// assert on the batch's size rather than its shape.
	_, cmd := m.Update(tickMsg(time.Now()))
	if cmd == nil {
		t.Fatal("a tick scheduled nothing; polling would stop")
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok {
		t.Fatalf("tick produced %T, want a batch of refresh + next tick", cmd())
	}
	if len(batch) != 2 {
		t.Errorf("tick scheduled %d commands, want exactly 2 (refresh + one successor tick)", len(batch))
	}

	// The clock tick carries no query, so its single successor must be the
	// only thing it returns.
	mm, clockCmd := m.Update(clockTickMsg(time.Now()))
	if clockCmd == nil {
		t.Fatal("a clock tick scheduled nothing; the Age column would freeze")
	}
	if _, isBatch := clockCmd().(tea.BatchMsg); isBatch {
		t.Error("the clock tick scheduled more than one command")
	}
	_ = mm
}

// TestIdleClockTickRescheduatesSlow pins the Update branch, not just the pure
// helper: an idle clock tick must come back at the slow cadence. Timed rather
// than type-asserted because a tea.Tick command is opaque — the slow one simply
// must not have fired by the time the fast one would have.
func TestIdleClockTickReschedulesSlow(t *testing.T) {
	m := testModel(t)
	m.lastActivity = time.Now().Add(-2 * idleBackoffAfter)
	_, cmd := m.Update(clockTickMsg(time.Now()))
	if cmd == nil {
		t.Fatal("no successor clock tick")
	}
	done := make(chan struct{})
	go func() { cmd(); close(done) }()
	select {
	case <-done:
		t.Error("the idle clock tick fired at the fast cadence")
	case <-time.After(fastClockInterval + 500*time.Millisecond):
		// Still pending well past the fast interval: it was scheduled slow.
	}
}

// TestFingerprintIgnoresAgentOrder is the reason activityFingerprint sorts:
// herdr does not promise a stable order for the agent list, and a reordered
// list is not an event. Without the sort the TUI would treat every reshuffle as
// activity and never back off, making the whole feature dead weight.
func TestFingerprintIgnoresAgentOrder(t *testing.T) {
	agents := []domain.AgentTransition{
		{AgentID: "w1:p1", AgentType: "claude", Status: "idle"},
		{AgentID: "w2:p9", AgentType: "codex", Status: "working"},
		{AgentID: "w3:p4", AgentType: "claude", Status: "blocked"},
	}
	forward := refreshMsg{status: frontend.Status{MonitoredAgents: agents}}
	reversed := refreshMsg{status: frontend.Status{MonitoredAgents: []domain.AgentTransition{
		agents[2], agents[1], agents[0],
	}}}
	if activityFingerprint(forward) != activityFingerprint(reversed) {
		t.Error("a reordered agent list changed the fingerprint; the poll would never back off")
	}
	// A real status change still moves it.
	changed := refreshMsg{status: frontend.Status{MonitoredAgents: []domain.AgentTransition{
		agents[0], agents[1], {AgentID: "w3:p4", AgentType: "claude", Status: "idle"},
	}}}
	if activityFingerprint(forward) == activityFingerprint(changed) {
		t.Error("a status change did not move the fingerprint")
	}
}

// TestTaskProgressCountsAsActivity: an agent working through a checklist writes
// no audit row and holds one status, so item-level task movement is the only
// signal that the Tasks tab — which the operator can watch change — is live.
func TestTaskProgressCountsAsActivity(t *testing.T) {
	group := func(marks ...string) refreshMsg {
		items := make([]domain.ChecklistItem, len(marks))
		for i, mk := range marks {
			items[i] = domain.ChecklistItem{Index: i, Mark: mk, Text: "task"}
		}
		return refreshMsg{tasks: []frontend.TaskGroup{{Index: 0, Items: items}}}
	}
	pending := group(" ", " ", " ")
	if activityFingerprint(pending) != activityFingerprint(group(" ", " ", " ")) {
		t.Fatal("identical task lists produced different fingerprints")
	}
	if activityFingerprint(pending) == activityFingerprint(group(" ", "-", " ")) {
		t.Error("an item moving to in-progress did not count as activity")
	}
	if activityFingerprint(pending) == activityFingerprint(group(" ", "x", " ")) {
		t.Error("an item being completed did not count as activity")
	}
	if activityFingerprint(pending) == activityFingerprint(group(" ", " ")) {
		t.Error("an item being removed did not count as activity")
	}
}
