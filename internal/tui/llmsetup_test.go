package tui

import (
	"context"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/llm"
)

var (
	claudeReady    = llm.CLIReadiness{Name: llm.CLIClaude, Ready: true}
	codexReady     = llm.CLIReadiness{Name: llm.CLICodex, Ready: true}
	claudeMissing  = llm.CLIReadiness{Name: llm.CLIClaude, Reason: llm.ReasonNotInstalled}
	codexLoggedOut = llm.CLIReadiness{Name: llm.CLICodex, Reason: llm.ReasonNotLoggedIn}
)

// setupModel is presetModel (a real config.toml behind a real App) with the
// first-run offer enabled, as run() does, and a fake probe that counts calls.
func setupModel(t *testing.T, results ...llm.CLIReadiness) (Model, *frontend.App, *atomic.Int32) {
	t.Helper()
	orig := llmSetupArmDelay
	llmSetupArmDelay = 0
	t.Cleanup(func() { llmSetupArmDelay = orig })
	m, app := presetModel(t)
	m.tab = tabAgents
	m.llmSetup.enabled = true
	probes := &atomic.Int32{}
	app.DetectLLM = func(context.Context, llm.EnvSpec) []llm.CLIReadiness {
		probes.Add(1)
		return results
	}
	return m, app, probes
}

// refreshOnce runs one real refresh through Update and returns the command
// the handler produced (the probe, when one starts).
func refreshOnce(t *testing.T, m Model) (Model, tea.Cmd) {
	t.Helper()
	upd, cmd := m.Update(m.refresh()())
	return upd.(Model), cmd
}

// runProbeCmd executes the probe command and feeds its result back.
func runProbeCmd(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	if cmd == nil {
		t.Fatal("refresh started no LLM probe")
	}
	msg, ok := cmd().(llmDetectedMsg)
	if !ok {
		t.Fatalf("refresh command returned %T, want llmDetectedMsg", msg)
	}
	upd, _ := m.Update(msg)
	return upd.(Model)
}

func loadCfg(t *testing.T, app *frontend.App) config.Config {
	t.Helper()
	cfg, err := config.Load(app.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// TestLLMSetupOffersClaudeFirstAndInstallsAllFourKeys is the feature end to
// end: a fresh config, both CLIs ready → the picker offers claude first with
// the highlight on it, and enter writes every [llm] recipe to the FILE.
func TestLLMSetupOffersClaudeFirstAndInstallsAllFourKeys(t *testing.T) {
	m, app, probes := setupModel(t, claudeReady, codexReady)
	m, cmd := refreshOnce(t, m)
	if m.prompt != nil {
		t.Fatal("offer opened before the probe answered")
	}
	m = runProbeCmd(t, m, cmd)
	if m.prompt == nil {
		t.Fatalf("no offer with claude and codex ready (message %q)", m.message)
	}
	if want := []string{"claude", "codex", llmSetupDecline}; !reflect.DeepEqual(m.prompt.options, want) {
		t.Fatalf("options = %v, want %v", m.prompt.options, want)
	}
	if m.prompt.optIdx != 0 || m.prompt.multi {
		t.Errorf("highlight %d multi %v, want single-select on claude", m.prompt.optIdx, m.prompt.multi)
	}
	if !strings.Contains(m.prompt.label, "edits AUTO.md") {
		t.Errorf("label %q does not warn that learning writes a file", m.prompt.label)
	}

	upd, submit := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = upd.(Model)
	if submit == nil {
		t.Fatal("enter ran nothing")
	}
	res, ok := submit().(actionResultMsg)
	if !ok || res.err != nil {
		t.Fatalf("install result = %#v", res)
	}
	cfg := loadCfg(t, app)
	for _, key := range frontend.LLMSetupKeys {
		want, _ := frontend.LLMPreset(key, frontend.LLMPresetClaude)
		if got := frontend.LLMCommandArgv(cfg, key); !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %#v, want the claude recipe", key, got)
		}
	}
	if len(cfg.FullSelfPrompting.OrchestratorAgentCommand) != 0 {
		t.Error("setup configured the orchestrator")
	}
	// One probe for the session, and no second offer on later refreshes.
	upd, _ = m.Update(res)
	m = upd.(Model)
	for range 2 {
		var c tea.Cmd
		m, c = refreshOnce(t, m)
		if c != nil {
			t.Error("a later refresh started another probe")
		}
	}
	if n := probes.Load(); n != 1 {
		t.Errorf("probed %d times, want 1", n)
	}
}

func TestLLMSetupOffersOnlyReadyCLIs(t *testing.T) {
	m, app, _ := setupModel(t, claudeMissing, codexReady)
	m, cmd := refreshOnce(t, m)
	m = runProbeCmd(t, m, cmd)
	if m.prompt == nil {
		t.Fatal("no offer with codex ready")
	}
	if want := []string{"codex", llmSetupDecline}; !reflect.DeepEqual(m.prompt.options, want) {
		t.Fatalf("options = %v, want %v", m.prompt.options, want)
	}
	upd, submit := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	_ = upd
	if res := submit().(actionResultMsg); res.err != nil {
		t.Fatal(res.err)
	}
	want, _ := frontend.LLMPreset(frontend.LLMCommandKey, frontend.LLMPresetCodex)
	if got := loadCfg(t, app).LLM.Command; !reflect.DeepEqual(got, want) {
		t.Errorf("llm.command = %v, want the codex recipe", got)
	}
}

func TestLLMSetupSilentWhenNothingReady(t *testing.T) {
	m, _, _ := setupModel(t, claudeMissing, codexLoggedOut)
	m, cmd := refreshOnce(t, m)
	m = runProbeCmd(t, m, cmd)
	if m.prompt != nil {
		t.Fatalf("offer opened with nothing ready: %v", m.prompt.options)
	}
	if m.message != "" {
		t.Errorf("message %q, want silence", m.message)
	}
}

// TestLLMSetupSkippedWhenAnyKeyConfigured: one configured key is the
// operator's own choice — no probe is spawned and nothing is offered.
func TestLLMSetupSkippedWhenAnyKeyConfigured(t *testing.T) {
	m, app, probes := setupModel(t, claudeReady)
	if _, err := app.ApplyLLMPreset(context.Background(), frontend.LLMRerankingCommandKey, frontend.LLMPresetCodex); err != nil {
		t.Fatal(err)
	}
	m, cmd := refreshOnce(t, m)
	if cmd != nil {
		t.Fatal("a configured LLM still started a probe")
	}
	// Clearing it later in the session does not re-open the question.
	if _, err := app.ClearLLMCommand(context.Background(), frontend.LLMRerankingCommandKey,
		frontend.LLMCommandArgv(loadCfg(t, app), frontend.LLMRerankingCommandKey)); err != nil {
		t.Fatal(err)
	}
	if _, cmd = refreshOnce(t, m); cmd != nil {
		t.Error("clearing the key mid-session started a probe")
	}
	if probes.Load() != 0 {
		t.Error("probe ran")
	}
}

// TestLLMSetupNeverOpensAfterAKeypress: the offer arrives asynchronously, so
// an enter meant for something else must never land on it. A keypress before
// the probe answers turns the popup into a one-line hint.
func TestLLMSetupNeverOpensAfterAKeypress(t *testing.T) {
	m, app, _ := setupModel(t, claudeReady)
	m, cmd := refreshOnce(t, m)
	m = press(t, m, "down")
	m = runProbeCmd(t, m, cmd)
	if m.prompt != nil {
		t.Fatalf("offer opened after a keypress: %v", m.prompt.options)
	}
	if !strings.Contains(m.message, "claude is ready") || !strings.Contains(m.message, "--preset auto") {
		t.Errorf("hint = %q", m.message)
	}
	if !frontend.LLMUnconfigured(loadCfg(t, app)) {
		t.Error("config changed without an answer")
	}
}

// TestLLMSetupWaitsForAnOpenModal: an offer never replaces a prompt the
// operator did not open themselves; a later refresh opens it once the modal
// closes (no key pressed — e.g. a prompt opened by a message).
func TestLLMSetupWaitsForAnOpenModal(t *testing.T) {
	m, _, _ := setupModel(t, claudeReady)
	m, cmd := refreshOnce(t, m)
	other := &prompt{label: "something else", options: []string{"a", "b"}, onSubmit: func(string) tea.Cmd { return nil }}
	m.openPrompt(other)
	m = runProbeCmd(t, m, cmd)
	if m.prompt != other {
		t.Fatal("offer replaced an open prompt")
	}
	m.prompt = nil
	m, _ = refreshOnce(t, m)
	if m.prompt == nil || m.prompt.options[0] != "claude" {
		t.Fatal("offer did not open once the modal closed")
	}
}

// TestLLMSetupProbeBeforeFirstRefresh: the probe and the first good refresh
// can arrive in either order.
func TestLLMSetupProbeBeforeFirstRefresh(t *testing.T) {
	m, _, _ := setupModel(t, claudeReady)
	upd, _ := m.Update(llmDetectedMsg{results: []llm.CLIReadiness{claudeReady}})
	m = upd.(Model)
	if m.prompt != nil {
		t.Fatal("offer opened before any refresh")
	}
	m, _ = refreshOnce(t, m)
	if m.prompt == nil {
		t.Fatal("offer did not open on the first refresh after the probe")
	}
}

// TestLLMSetupDeclineIsSessionOnly: "not now" writes nothing and does not ask
// again this session.
func TestLLMSetupDeclineIsSessionOnly(t *testing.T) {
	m, app, _ := setupModel(t, claudeReady)
	m, cmd := refreshOnce(t, m)
	m = runProbeCmd(t, m, cmd)
	m = press(t, m, "down")
	upd, submit := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = upd.(Model)
	res := submit().(actionResultMsg)
	if res.err != nil || !strings.Contains(res.message, "--preset auto") {
		t.Fatalf("decline result = %#v", res)
	}
	upd, _ = m.Update(res)
	m = upd.(Model)
	m, _ = refreshOnce(t, m)
	if m.prompt != nil {
		t.Error("offer reopened after not now")
	}
	if !frontend.LLMUnconfigured(loadCfg(t, app)) {
		t.Error("not now wrote config")
	}
}

// TestLLMSetupOffForPlainModels: a Model built by New (every other unit test)
// never probes — only run() turns the offer on.
func TestLLMSetupOffForPlainModels(t *testing.T) {
	m, app := presetModel(t)
	app.DetectLLM = func(context.Context, llm.EnvSpec) []llm.CLIReadiness {
		t.Fatal("a plain Model probed")
		return nil
	}
	if _, cmd := refreshOnce(t, m); cmd != nil {
		t.Error("a plain Model's refresh started a probe")
	}
}

// TestLLMSetupSwallowsKeysUntilArmed: a key pressed just after the offer
// draws was aimed at the screen it covered — it must not answer the offer.
func TestLLMSetupSwallowsKeysUntilArmed(t *testing.T) {
	m, app, _ := setupModel(t, claudeReady)
	llmSetupArmDelay = time.Hour
	m, cmd := refreshOnce(t, m)
	m = runProbeCmd(t, m, cmd)
	if m.prompt == nil {
		t.Fatal("no offer")
	}
	for _, k := range []tea.KeyMsg{{Type: tea.KeyEnter}, {Type: tea.KeyDown}, {Type: tea.KeyEsc}} {
		upd, c := m.Update(k)
		m = upd.(Model)
		if c != nil || m.prompt == nil || m.prompt.optIdx != 0 {
			t.Fatalf("%v reached the unarmed offer (prompt %v)", k, m.prompt)
		}
	}
	if !frontend.LLMUnconfigured(loadCfg(t, app)) {
		t.Fatal("an unarmed enter wrote config")
	}
	// Once armed, enter answers it.
	m.prompt.armedAt = time.Time{}
	if _, c := m.Update(tea.KeyMsg{Type: tea.KeyEnter}); c == nil {
		t.Fatal("armed enter ran nothing")
	} else if res := c().(actionResultMsg); res.err != nil {
		t.Fatal(res.err)
	}
	if frontend.LLMUnconfigured(loadCfg(t, app)) {
		t.Error("armed enter did not install")
	}
}
