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

// fspTestApp wires a real frontend.App whose enable preconditions hold,
// so the toggle's cmd can run end to end against the real SetFullSelfPrompting.
func fspTestApp(t *testing.T) *frontend.App {
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
	for i := 0; i < config.MinFSPGraduatedRules; i++ {
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

// TestDoubleRTogglesFullSelfPrompting: two r inside the window call the real
// SetFullSelfPrompting, enable the mode, and report it; the deferred resume
// never runs.
func TestDoubleRTogglesFullSelfPrompting(t *testing.T) {
	m := testModel(t)
	m.app = fspTestApp(t)
	m.ctx = context.Background()

	m, cmd := pressOne(t, m, "r")
	if !m.doubleRArmed || cmd == nil {
		t.Fatalf("first r must arm and schedule the window timer (armed=%v cmd=%v)", m.doubleRArmed, cmd)
	}
	staleSeq := m.doubleRSeq

	m, cmd = pressOne(t, m, "r")
	if m.doubleRArmed {
		t.Fatal("second r must disarm")
	}
	if cmd == nil {
		t.Fatal("second r must dispatch the toggle")
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
	if !strings.Contains(res.message, "full self-prompting ON") {
		t.Errorf("toggle message = %q, want full self-prompting ON", res.message)
	}
	cfg, err := m.app.Config()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.FullSelfPrompting.Enabled {
		t.Fatal("double-r did not enable full self-prompting in config")
	}

	// The first press's timer is stale now and must dispatch NOTHING.
	// Assert on the COMMAND: neither branch writes m.message (the live branch
	// calls beginAction, which clears it), so a message assertion here would
	// pass even with the guard deleted.
	_, cmd = m.Update(doubleRTimerMsg{seq: staleSeq})
	if cmd != nil {
		t.Error("the superseded timer dispatched an action; it must be inert")
	}
}

// TestStaleDoubleRTimerIsInertWhileArmed exercises the SEQ guard specifically.
// The disarm check alone cannot catch this: here the model IS armed again (a
// fresh r), so only the sequence number distinguishes the old timer from the
// live one. Without it, a timer from an abandoned gesture resumes automation
// the operator never asked to resume.
func TestStaleDoubleRTimerIsInertWhileArmed(t *testing.T) {
	m := testModel(t)
	m.app = fspTestApp(t)
	m.ctx = context.Background()

	m, _ = pressOne(t, m, "r") // arm; seq = S
	staleSeq := m.doubleRSeq
	m, _ = pressOne(t, m, "j") // disarm
	m, _ = pressOne(t, m, "r") // arm again; seq > S
	if !m.doubleRArmed {
		t.Fatal("expected the model to be armed again")
	}

	_, cmd := m.Update(doubleRTimerMsg{seq: staleSeq})
	if cmd != nil {
		t.Error("a timer from the abandoned gesture fired while armed; the seq guard is not holding")
	}
	// The CURRENT timer still works.
	_, cmd = m.Update(doubleRTimerMsg{seq: m.doubleRSeq})
	if cmd == nil {
		t.Error("the live timer must still run the deferred resume")
	}
}

// TestModalDisarmsPendingDoubleR: a key that opens or feeds a modal must
// disarm, or the deferred resume fires later against automation the operator
// never asked to resume — while their attention is on the modal.
func TestModalDisarmsPendingDoubleR(t *testing.T) {
	for _, tc := range []struct {
		name string
		open func(m *Model)
	}{
		{"detail overlay", func(m *Model) { m.detail = &detailView{} }},
		{"confirm modal", func(m *Model) { m.confirm = &confirmation{label: "sure?"} }},
		{"text prompt", func(m *Model) { m.prompt = &prompt{} }},
		{"search mode", func(m *Model) { m.searching = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := testModel(t)
			m.app = fspTestApp(t)
			m.ctx = context.Background()

			m, _ = pressOne(t, m, "r")
			if !m.doubleRArmed {
				t.Fatal("first r must arm")
			}
			tc.open(&m)
			// An "r" now belongs to the modal, not to the gesture.
			upd, _ := m.Update(pressKeyMsg("r"))
			m = upd.(Model)
			if m.doubleRArmed {
				t.Fatal("a modal key must disarm the pending double-press")
			}
			if _, cmd := m.Update(doubleRTimerMsg{seq: m.doubleRSeq}); cmd != nil {
				t.Error("the dropped gesture still dispatched an action")
			}
		})
	}
}

// TestDoubleRTogglesFullSelfPromptingOff: the OFF transition is not symmetric
// with ON — it has no preconditions at all — so it gets its own case.
func TestDoubleRTogglesFullSelfPromptingOff(t *testing.T) {
	m := testModel(t)
	m.app = fspTestApp(t)
	m.ctx = context.Background()
	if err := m.app.SetFullSelfPrompting(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	m.data.status.FullSelfPrompting = true

	m, _ = pressOne(t, m, "r")
	_, cmd := pressOne(t, m, "r")
	if cmd == nil {
		t.Fatal("rr must dispatch the toggle")
	}
	res, ok := cmd().(actionResultMsg)
	if !ok || res.err != nil {
		t.Fatalf("toggle off failed: %+v", res)
	}
	if !strings.Contains(res.message, "OFF") {
		t.Errorf("message = %q, want it to report OFF", res.message)
	}
	cfg, err := m.app.Config()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FullSelfPrompting.Enabled {
		t.Error("rr did not disable the mode")
	}
}

// TestSingleRStillResumesAfterTheWindow: one r defers, and when the window
// expires the original single-press action — resume — runs. The deferral is
// what lets rr mean something else without stealing r's meaning.
func TestSingleRStillResumesAfterTheWindow(t *testing.T) {
	m := testModel(t)
	m.app = fspTestApp(t)
	m.ctx = context.Background()

	m, _ = pressOne(t, m, "r")
	upd, cmd := m.Update(doubleRTimerMsg{seq: m.doubleRSeq})
	m = upd.(Model)
	if m.doubleRArmed {
		t.Fatal("timer must disarm")
	}
	if cmd == nil {
		t.Fatal("the expired window must run the deferred resume")
	}
	res, ok := cmd().(actionResultMsg)
	if !ok {
		t.Fatalf("deferred action returned %T, want actionResultMsg", cmd())
	}
	if res.err != nil || !strings.Contains(res.message, "resumed") {
		t.Errorf("deferred action = %+v, want a resume result", res)
	}
}

// TestInterveningKeyDisarmsDoubleR: r, j, r is two singles — the second R
// arms a fresh cycle instead of toggling.
func TestInterveningKeyDisarmsDoubleR(t *testing.T) {
	m := testModel(t)
	m.app = fspTestApp(t)
	m.ctx = context.Background()

	m, _ = pressOne(t, m, "r")
	m, _ = pressOne(t, m, "j")
	if m.doubleRArmed {
		t.Fatal("j must disarm the pending double-press")
	}
	m, _ = pressOne(t, m, "r")
	if !m.doubleRArmed {
		t.Fatal("r after a disarm must arm again, not toggle")
	}
	cfg, err := m.app.Config()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FullSelfPrompting.Enabled {
		t.Fatal("r,j,r toggled full self-prompting")
	}
}

// TestDoubleRWhilePausedResumesInsteadOfToggling: enabling is refused while
// paused, so rr must not swallow both presses into a refusal that leaves
// automation still paused — an operator hitting r twice wants what r does.
// It resumes, and says the mode can be enabled next.
func TestDoubleRWhilePausedResumesInsteadOfToggling(t *testing.T) {
	m := testModel(t)
	m.app = fspTestApp(t)
	m.ctx = context.Background()
	m.data.status.Paused = true

	m, _ = pressOne(t, m, "r")
	m, cmd := pressOne(t, m, "r")
	if cmd == nil {
		t.Fatal("rr while paused must still resume")
	}
	res, ok := cmd().(actionResultMsg)
	if !ok {
		t.Fatalf("returned %T, want actionResultMsg", cmd())
	}
	if res.err != nil {
		t.Fatalf("resume failed: %v", res.err)
	}
	if !strings.Contains(res.message, "resumed") {
		t.Errorf("message = %q, want it to report the resume", res.message)
	}
	// And the mode was NOT enabled behind the operator's back.
	cfg, err := m.app.Config()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FullSelfPrompting.Enabled {
		t.Error("rr while paused enabled the mode; enabling is refused while paused")
	}
}

// TestHeaderShowsFullSelfPromptingAndPausedWins: the header segment reflects the mode,
// and the kill switch outranks it.
func TestHeaderShowsFullSelfPromptingAndPausedWins(t *testing.T) {
	m := testModel(t)
	m.data.status.FullSelfPrompting = true
	if v := m.View(); !strings.Contains(v, "⚡ FULL SELF-PROMPTING") {
		t.Error("header does not show the full self-prompting segment")
	}
	m.data.status.Paused = true
	v := m.View()
	if strings.Contains(v, "⚡ FULL SELF-PROMPTING") || !strings.Contains(v, "PAUSED") {
		t.Error("paused must win over the full self-prompting segment")
	}
}

// TestFullSelfPromptingBlockedBanner: configured-on-but-inactive renders the warning
// line with its reason.
func TestFullSelfPromptingBlockedBanner(t *testing.T) {
	m := testModel(t)
	m.data.status.FullSelfPrompting = true
	m.data.status.FullSelfPromptingBlocked = "only 4 of 10 required graduated (autonomous) rules remain"
	if v := m.View(); !strings.Contains(v, "full self-prompting is ON but inactive: only 4 of 10") {
		t.Error("blocked banner missing")
	}
	m.data.status.FullSelfPromptingBlocked = ""
	if v := m.View(); strings.Contains(v, "ON but inactive") {
		t.Error("banner rendered with no blockage")
	}
}

// TestHelpLineAdvertisesDoubleR: the shortcut is discoverable.
func TestHelpLineAdvertisesDoubleR(t *testing.T) {
	m := testModel(t)
	if h := m.helpLine(); !strings.Contains(h, "rr: full self-prompting") {
		t.Errorf("help line %q does not advertise RR", h)
	}
}
