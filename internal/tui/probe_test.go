package tui

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

// probeModel is a model over a real sqlite store that has taken its first,
// full refresh — the state every later tick starts from.
func probeModel(t *testing.T, st ports.StorePort) (Model, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	raw, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	if st == nil {
		st = raw
	}
	app := &frontend.App{Store: st, ConfigPath: seedLocalFSConfigIn(t, dir), Author: "op", StateDir: dir}
	m := New(context.Background(), app)
	m.width, m.height = 100, 30
	first, ok := m.refresh()().(refreshMsg)
	if !ok || first.err != nil {
		t.Fatalf("first refresh: %#v", first.err)
	}
	mm, _ := m.Update(first)
	return mm.(Model), raw
}

// TestAnUnchangedStoreIsProbedNotReRead pins the TUI's idle cost: a tick over
// data that has not moved asks for the change key and re-reads NOTHING, while
// a write is picked up on the very next tick. Serving the old unconditional
// 2s re-read was most of the daemon's CPU while a TUI was open.
func TestAnUnchangedStoreIsProbedNotReRead(t *testing.T) {
	m, st := probeModel(t, nil)
	if m.lastChangeKey == "" {
		t.Fatal("a refresh over a sqlite store recorded no change key")
	}
	poll := func(at time.Time) tea.Msg { return m.poll(at)() }

	if got, ok := poll(time.Now()).(probeMsg); !ok {
		t.Fatalf("a tick over unchanged data produced %T, want probeMsg", got)
	}

	// The control: a write must be a full refresh that carries it.
	id, err := st.AppendAudit(context.Background(), domain.AuditRecord{
		AgentID: "a1", SituationType: domain.SituationApproval, Trigger: "t",
		Action: "escalated", Status: "escalated", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := poll(time.Now()).(refreshMsg)
	if !ok {
		t.Fatalf("a tick after a store write produced %T, want refreshMsg", poll(time.Now()))
	}
	if len(got.escalations) != 1 || got.escalations[0].ID != id {
		t.Errorf("the refresh after the write carried %v, want escalation %d", got.escalations, id)
	}
	mm, _ := m.Update(got)
	m = mm.(Model)
	if _, ok := poll(time.Now()).(probeMsg); !ok {
		t.Error("the tick after that refresh re-read again; the key did not settle")
	}

	// What a refresh derives from the clock is re-read on the backstop.
	if _, ok := poll(time.Now().Add(refreshBackstop)).(refreshMsg); !ok {
		t.Error("the backstop did not force a full refresh")
	}
}

// TestAProbeRefreshesHealthButIsNotActivity: a probe still carries what a poll
// reads besides the data, and finding nothing changed is exactly the quiet the
// idle backoff waits for — it must not reset the idle clock.
func TestAProbeRefreshesHealthButIsNotActivity(t *testing.T) {
	m, _ := probeModel(t, nil)
	quietSince := time.Now().Add(-time.Hour)
	m.lastActivity = quietSince
	health := frontend.DaemonHealth{Running: true, PID: 4242}
	mm, cmd := m.Update(probeMsg{health: health})
	m = mm.(Model)
	if cmd != nil {
		t.Errorf("a probe scheduled %T; it must not trigger more work", cmd())
	}
	if m.data.daemonHealth.PID != 4242 {
		t.Errorf("daemon health = %+v, want the probe's", m.data.daemonHealth)
	}
	if !m.lastActivity.Equal(quietSince) {
		t.Error("a probe that found nothing reset the idle clock")
	}
}

// TestAStoreWithoutAChangeKeyIsReReadEveryTick is the fallback: a store that
// cannot report a change token gets the old behaviour, never a stale screen.
func TestAStoreWithoutAChangeKeyIsReReadEveryTick(t *testing.T) {
	dir := t.TempDir()
	raw, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	m, _ := probeModel(t, struct{ ports.StorePort }{raw})
	if m.lastChangeKey != "" {
		t.Fatalf("a store without a change token recorded key %q", m.lastChangeKey)
	}
	if got, ok := m.poll(time.Now())().(refreshMsg); !ok {
		t.Errorf("a tick without a change key produced %T, want refreshMsg", got)
	}
}
