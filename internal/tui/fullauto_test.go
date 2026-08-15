package tui

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

// fullAutoTestApp wires a real frontend.App whose enable preconditions hold,
// so the toggle's cmd can run end to end against the real SetFullAuto.
func fullAutoTestApp(t *testing.T) *frontend.App {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	app := &frontend.App{
		Store:      st,
		ConfigPath: filepath.Join(dir, "config.toml"),
		Author:     "operator",
	}
	ctx := context.Background()
	if _, err := app.SetField(ctx, "llm.command", `claude -p "decide"`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < config.MinFullAutoGraduatedRules; i++ {
		if err := st.UpsertSignature(ctx, domain.SignatureState{
			Signature: fmt.Sprintf("approval:tui-grad-%d", i), SituationType: domain.SituationApproval,
			AgentType: "claude", Mode: domain.ModeAutonomous, UpdatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	return app
}

func pressOne(t *testing.T, m Model, key string) (Model, tea.Cmd) {
	t.Helper()
	upd, cmd := m.Update(pressKeyMsg(key))
	return upd.(Model), cmd
}

// TestDoubleRTogglesFullAuto: two R inside the window call the real
// SetFullAuto, enable the mode, and report it; the deferred re-embed action
// never fires.
func TestDoubleRTogglesFullAuto(t *testing.T) {
	m := testModel(t)
	m.app = fullAutoTestApp(t)
	m.ctx = context.Background()

	m, cmd := pressOne(t, m, "R")
	if !m.reembedArmed || cmd == nil {
		t.Fatalf("first R must arm and schedule the window timer (armed=%v cmd=%v)", m.reembedArmed, cmd)
	}
	staleSeq := m.reembedSeq

	m, cmd = pressOne(t, m, "R")
	if m.reembedArmed {
		t.Fatal("second R must disarm")
	}
	if cmd == nil {
		t.Fatal("second R must dispatch the toggle")
	}
	// A tea.Cmd is one-shot: invoke exactly once, assert on the value.
	msg := cmd()
	res, ok := msg.(actionResultMsg)
	if !ok {
		t.Fatalf("toggle cmd returned %T, want actionResultMsg", msg)
	}
	if res.err != nil {
		t.Fatalf("toggle failed: %v", res.err)
	}
	if !strings.Contains(res.message, "full-auto ON") {
		t.Errorf("toggle message = %q, want full-auto ON", res.message)
	}
	cfg, err := m.app.Config()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Escalations.FullAuto.Enabled {
		t.Fatal("double-R did not enable full-auto in config")
	}

	// The first press's deferred timer is stale now: it must NOT re-embed.
	upd, _ := m.Update(reembedTimerMsg{seq: staleSeq})
	m = upd.(Model)
	if m.message != "" {
		t.Errorf("stale timer produced output: %q", m.message)
	}
}

// TestSingleRStillReembedsAfterTheWindow: one R defers, and the timer fires
// the original single-press behavior (here: the no-drift refusal message).
func TestSingleRStillReembedsAfterTheWindow(t *testing.T) {
	m := testModel(t)

	m, _ = pressOne(t, m, "R")
	upd, _ := m.Update(reembedTimerMsg{seq: m.reembedSeq})
	m = upd.(Model)
	if m.reembedArmed {
		t.Fatal("timer must disarm")
	}
	if !strings.Contains(m.message, "no embedding drift detected") {
		t.Errorf("single R message = %q, want the no-drift notice", m.message)
	}
}

// TestInterveningKeyDisarmsDoubleR: R, j, R is two singles — the second R
// arms a fresh cycle instead of toggling.
func TestInterveningKeyDisarmsDoubleR(t *testing.T) {
	m := testModel(t)
	m.app = fullAutoTestApp(t)
	m.ctx = context.Background()

	m, _ = pressOne(t, m, "R")
	m, _ = pressOne(t, m, "j")
	if m.reembedArmed {
		t.Fatal("j must disarm the pending double-press")
	}
	m, _ = pressOne(t, m, "R")
	if !m.reembedArmed {
		t.Fatal("R after a disarm must arm again, not toggle")
	}
	cfg, err := m.app.Config()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Escalations.FullAuto.Enabled {
		t.Fatal("R,j,R toggled full-auto")
	}
}

// TestDoubleRRefusedWhilePaused: the TUI refuses locally, without dispatching.
func TestDoubleRRefusedWhilePaused(t *testing.T) {
	m := testModel(t)
	m.data.status.Paused = true

	m, _ = pressOne(t, m, "R")
	m, cmd := pressOne(t, m, "R")
	if cmd != nil {
		t.Fatal("paused enable must not dispatch anything")
	}
	if !strings.Contains(m.message, "paused") {
		t.Errorf("refusal = %q, want it to name the pause", m.message)
	}
}

// TestHeaderShowsFullAutoAndPausedWins: the header segment reflects the mode,
// and the kill switch outranks it.
func TestHeaderShowsFullAutoAndPausedWins(t *testing.T) {
	m := testModel(t)
	m.data.status.FullAuto = true
	if v := m.View(); !strings.Contains(v, "⚡ FULL-AUTO") {
		t.Error("header does not show the full-auto segment")
	}
	m.data.status.Paused = true
	v := m.View()
	if strings.Contains(v, "⚡ FULL-AUTO") || !strings.Contains(v, "PAUSED") {
		t.Error("paused must win over the full-auto segment")
	}
}

// TestFullAutoBlockedBanner: configured-on-but-inactive renders the warning
// line with its reason.
func TestFullAutoBlockedBanner(t *testing.T) {
	m := testModel(t)
	m.data.status.FullAuto = true
	m.data.status.FullAutoBlocked = "only 4 of 10 required graduated (autonomous) rules remain"
	if v := m.View(); !strings.Contains(v, "full-auto is ON but inactive: only 4 of 10") {
		t.Error("blocked banner missing")
	}
	m.data.status.FullAutoBlocked = ""
	if v := m.View(); strings.Contains(v, "ON but inactive") {
		t.Error("banner rendered with no blockage")
	}
}

// TestHelpLineAdvertisesDoubleR: the shortcut is discoverable.
func TestHelpLineAdvertisesDoubleR(t *testing.T) {
	m := testModel(t)
	if h := m.helpLine(); !strings.Contains(h, "RR: full-auto") {
		t.Errorf("help line %q does not advertise RR", h)
	}
}
