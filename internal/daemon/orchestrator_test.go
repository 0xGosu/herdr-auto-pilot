package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

const orchestratorOn = "[full_self_prompting]\nenabled = true\n" +
	"orchestrator_agent_command = [\"claude\", \"--model\", \"opus\"]\n"

// orchLauncher adds ports.AgentLauncher and a visible-pane read to the fake
// herdr. A started agent joins the fake's listing, the way herdr's does.
type orchLauncher struct {
	*fakeHerdr
	lmu       sync.Mutex
	named     map[string]domain.AgentTransition
	visible   string
	created   []string
	started   [][]string
	lookups   int
	failStart bool
}

var (
	_ ports.AgentLauncher     = (*orchLauncher)(nil)
	_ ports.VisiblePaneReader = (*orchLauncher)(nil)
)

func (l *orchLauncher) AgentByName(_ context.Context, name string) (domain.AgentTransition, bool, error) {
	l.lmu.Lock()
	defer l.lmu.Unlock()
	l.lookups++
	a, ok := l.named[name]
	return a, ok, nil
}

func (l *orchLauncher) NewPaneInWorkspace(_ context.Context, workspace, tab, cwd string) (string, error) {
	l.lmu.Lock()
	defer l.lmu.Unlock()
	l.created = append(l.created, workspace+"|"+tab+"|"+cwd)
	return "wO:p1", nil
}

func (l *orchLauncher) StartAgent(_ context.Context, name, kind, pane string, args []string) error {
	l.lmu.Lock()
	l.started = append(l.started, append([]string{kind}, args...))
	fail := l.failStart
	l.lmu.Unlock()
	if fail {
		return errors.New("induced start failure")
	}
	a := domain.AgentTransition{AgentID: pane, PaneID: pane, WorkspaceID: "wO", AgentType: kind,
		Status: "idle", TerminalID: "term_orch"}
	l.lmu.Lock()
	l.named[name] = a
	l.lmu.Unlock()
	l.mu.Lock()
	l.agents = append(l.agents, a)
	l.mu.Unlock()
	return nil
}

func (l *orchLauncher) ReadPaneVisible(context.Context, string, int) (string, error) {
	l.lmu.Lock()
	defer l.lmu.Unlock()
	return l.visible, nil
}

func (l *orchLauncher) setVisible(pane string) {
	l.lmu.Lock()
	defer l.lmu.Unlock()
	l.visible = pane
}

func (l *orchLauncher) snapshot() (created []string, started [][]string, lookups int) {
	l.lmu.Lock()
	defer l.lmu.Unlock()
	return append([]string(nil), l.created...), append([][]string(nil), l.started...), l.lookups
}

// emptyComposer is a real Claude pane parked at an empty composer.
func emptyComposer(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("../domain/testdata/claude_session_untitled.txt")
	if err != nil {
		t.Fatal(err)
	}
	if s, ok := domain.ClaudeSessionFromPane(string(data)); !ok || !s.ComposerEmpty {
		t.Fatal("fixture no longer reads as an empty composer")
	}
	return string(data)
}

func newOrchHarness(t *testing.T, cfgTOML string, setup func(*orchLauncher)) (*harness, *orchLauncher, string) {
	t.Helper()
	composer := emptyComposer(t)
	state := t.TempDir()
	var l *orchLauncher
	fl := &fakeLLM{}
	h := newHarnessCore(t, cfgTOML, func(fh *fakeHerdr) ports.HerdrPort {
		l = &orchLauncher{fakeHerdr: fh, named: map[string]domain.AgentTransition{}, visible: composer}
		if setup != nil {
			setup(l)
		}
		return l
	}, fl, fl, nil, func(o *Options) {
		o.StateDir = state
		o.ResolveSelf = func() (string, error) { return "/opt/hap/bin/hap", nil }
	})
	return h, l, state
}

func waitOrchestratorIdle(t *testing.T, h *harness) {
	t.Helper()
	waitFor(t, 5*time.Second, func() bool {
		h.daemon.orch.mu.Lock()
		defer h.daemon.orch.mu.Unlock()
		return !h.daemon.orch.running
	})
}

// Configured with the mode on, the daemon creates the session in its own
// workspace, names and disables it, and briefs it — all from startup, with no
// sweep involved.
func TestOrchestratorIsStartedNamedDisabledAndBriefed(t *testing.T) {
	h, l, state := newOrchHarness(t, orchestratorOn, nil)
	ctx := context.Background()
	waitFor(t, 5*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })

	created, started, _ := l.snapshot()
	wantCreated := domain.OrchestratorWorkspaceLabel + "|" + domain.OrchestratorAgentName + "|" +
		filepath.Join(state, orchestratorDirName)
	if len(created) != 1 || created[0] != wantCreated {
		t.Errorf("panes created = %q, want [%q]", created, wantCreated)
	}
	if len(started) != 1 || strings.Join(started[0], " ") != "claude --model opus" {
		t.Errorf("agent starts = %q, want one of kind claude with --model opus", started)
	}
	id := h.daemon.orchestratorIdentity()
	if id.PaneID != "wO:p1" || id.TerminalID != "term_orch" || !id.Briefed {
		t.Fatalf("identity = %+v", id)
	}
	if _, err := os.Stat(filepath.Join(state, orchestratorStateFile)); err != nil {
		t.Errorf("identity not persisted: %v", err)
	}
	names, err := h.raw.AgentNames(ctx)
	if err != nil || names["wO:p1"] != domain.OrchestratorAgentName {
		t.Errorf("hap name = %q (%v), want %q", names["wO:p1"], err, domain.OrchestratorAgentName)
	}
	if disabled, err := h.raw.AgentDisabled(ctx, "wO:p1"); err != nil || !disabled {
		t.Errorf("disabled = %v (%v), want true", disabled, err)
	}
	brief := h.herdr.sentInputs()[0]
	if !strings.Contains(brief, "/opt/hap/bin/hap stream orchestrator") || strings.Contains(brief, "{self}") {
		t.Errorf("brief did not expand {self}:\n%s", brief)
	}

	// A later pass over a listing that holds the live, briefed session does
	// nothing at all.
	waitOrchestratorIdle(t, h)
	agents, _ := h.herdr.ListAgents(ctx)
	h.daemon.startOrchestratorPass(agents)
	waitOrchestratorIdle(t, h)
	if _, started, _ := l.snapshot(); len(started) != 1 || len(h.herdr.sentInputs()) != 1 {
		t.Fatalf("a healthy orchestrator was started or briefed again: starts=%d sends=%d",
			len(started), len(h.herdr.sentInputs()))
	}
}

// Disabled is not enough: a disabled agent is still captured, classified and
// audited on every event. The orchestrator must produce nothing — while an
// ordinary agent beside it is processed as always (the control).
func TestOrchestratorEventsAreIgnored(t *testing.T) {
	h, _, _ := newOrchHarness(t, orchestratorOn, nil)
	ctx := context.Background()
	waitFor(t, 5*time.Second, func() bool { return h.daemon.orchestratorIdentity().Briefed })
	waitOrchestratorIdle(t, h)
	h.herdr.setPane(approvalPane)

	h.events.ch <- domain.AgentTransition{AgentID: "wO:p1", PaneID: "wO:p1", AgentType: "claude", Status: "blocked"}
	h.events.ch <- domain.AgentTransition{AgentID: "pB", PaneID: "pB", AgentType: "claude", Status: "blocked"}
	waitFor(t, 5*time.Second, func() bool {
		rows, _ := h.raw.AuditLog(ctx, 50)
		for _, r := range rows {
			if r.AgentID == "pB" {
				return true
			}
		}
		return false
	})
	rows, err := h.raw.AuditLog(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.AgentID == "wO:p1" {
			t.Fatalf("the orchestrator was processed: %+v", r)
		}
	}
	if !h.daemon.isOrchestrator(domain.AgentTransition{PaneID: "wO:p1"}) {
		t.Fatal("identity lost")
	}
}

// An agent herdr already calls "orchestrator" is adopted, never duplicated —
// and never briefed: hap did not start it.
func TestOrchestratorAdoptsAnExistingSessionWithoutBriefingIt(t *testing.T) {
	existing := domain.AgentTransition{AgentID: "wX:p9", PaneID: "wX:p9", AgentType: "claude",
		Status: "idle", TerminalID: "term_x"}
	h, l, _ := newOrchHarness(t, orchestratorOn, func(l *orchLauncher) {
		l.named[domain.OrchestratorAgentName] = existing
		l.agents = append(l.agents, existing)
	})
	waitFor(t, 5*time.Second, func() bool { return h.daemon.orchestratorIdentity().Known() })
	waitOrchestratorIdle(t, h)
	created, started, _ := l.snapshot()
	if len(created) != 0 || len(started) != 0 {
		t.Fatalf("a second session was created beside an existing one: created=%q started=%q", created, started)
	}
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Fatalf("an adopted session was typed into: %q", got)
	}
	if id := h.daemon.orchestratorIdentity(); id.PaneID != "wX:p9" || id.TerminalID != "term_x" {
		t.Fatalf("identity = %+v", id)
	}
}

// Every gate fails closed and creates nothing.
func TestOrchestratorGatesRefuse(t *testing.T) {
	on := config.Default()
	on.FullSelfPrompting.Enabled = true
	on.FullSelfPrompting.OrchestratorAgentCommand = []string{"claude"}

	t.Run("mode off or no command", func(t *testing.T) {
		for _, cfg := range []string{
			"[full_self_prompting]\nenabled = false\norchestrator_agent_command = [\"claude\"]\n",
			"[full_self_prompting]\nenabled = true\n",
		} {
			h, l, _ := newOrchHarness(t, cfg, nil)
			h.daemon.startOrchestratorPass(nil)
			waitOrchestratorIdle(t, h)
			if _, _, lookups := l.snapshot(); lookups != 0 {
				t.Errorf("config %q: the pass ran (%d lookups)", cfg, lookups)
			}
		}
	})
	t.Run("paused", func(t *testing.T) {
		h, l, _ := newOrchHarness(t, "", nil)
		ctx := context.Background()
		if _, err := h.raw.InsertKillEvent(ctx, domain.KillEvent{State: domain.KillStateActiveValue,
			Scope: domain.KillScopeGlobal, Author: "operator", CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		h.daemon.ensureOrchestrator(ctx, l, on, []domain.AgentTransition{})
		if _, _, lookups := l.snapshot(); lookups != 0 {
			t.Fatal("a paused herd looked up or started an orchestrator")
		}
	})
	t.Run("not claude", func(t *testing.T) {
		h, l, _ := newOrchHarness(t, "", nil)
		bad := on
		bad.FullSelfPrompting.OrchestratorAgentCommand = []string{"codex", "exec"}
		h.daemon.ensureOrchestrator(context.Background(), l, bad, []domain.AgentTransition{})
		if _, _, lookups := l.snapshot(); lookups != 0 {
			t.Fatal("a command herdr cannot start as claude was acted on")
		}
	})
	t.Run("stood down", func(t *testing.T) {
		h, l, _ := newOrchHarness(t, "", nil)
		h.daemon.mu.Lock()
		h.daemon.cfg.FullSelfPrompting = on.FullSelfPrompting
		h.daemon.fspCeilingLatched = true
		h.daemon.mu.Unlock()
		h.daemon.startOrchestratorPass([]domain.AgentTransition{})
		waitOrchestratorIdle(t, h)
		if _, _, lookups := l.snapshot(); lookups != 0 {
			t.Fatal("a mode stood down at its ceiling still started an orchestrator")
		}
	})
}

// A first-run prompt standing in the session defers the brief — nothing is
// typed into a modal — tells the operator once, and the brief goes out once
// the composer is ready.
func TestOrchestratorBriefWaitsForAReadyComposer(t *testing.T) {
	h, l, _ := newOrchHarness(t, orchestratorOn, func(l *orchLauncher) {
		l.visible = "Do you trust the files in this folder?\n❯ 1. Yes, proceed\n  2. No, exit\n"
	})
	waitFor(t, 5*time.Second, func() bool { return h.daemon.orchestratorIdentity().Known() })
	waitOrchestratorIdle(t, h)
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Fatalf("typed into a modal: %q", got)
	}
	h.herdr.mu.Lock()
	notified := len(h.herdr.notifications)
	h.herdr.mu.Unlock()
	if notified != 1 {
		t.Fatalf("notifications = %d, want 1", notified)
	}

	l.setVisible(emptyComposer(t))
	agents, _ := h.herdr.ListAgents(context.Background())
	h.daemon.startOrchestratorPass(agents)
	waitFor(t, 5*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	if id := h.daemon.orchestratorIdentity(); !id.Briefed || id.BriefAttempts != 1 {
		t.Fatalf("identity = %+v, want briefed on the first real attempt", id)
	}
}

// A start that herdr failed and that left no session is a failure with a
// backoff, never a tight retry loop.
func TestOrchestratorFailedStartBacksOff(t *testing.T) {
	h, l, _ := newOrchHarness(t, orchestratorOn, func(l *orchLauncher) { l.failStart = true })
	waitFor(t, 5*time.Second, func() bool {
		_, started, _ := l.snapshot()
		return len(started) == 1
	})
	waitOrchestratorIdle(t, h)
	h.daemon.startOrchestratorPass([]domain.AgentTransition{})
	waitOrchestratorIdle(t, h)
	if _, started, _ := l.snapshot(); len(started) != 1 {
		t.Fatalf("retried inside the backoff: %d starts", len(started))
	}
	if h.daemon.orchestratorIdentity().Known() {
		t.Fatal("a failed start recorded an identity")
	}
}

func TestOrchestratorSpawnRateCap(t *testing.T) {
	h, _, _ := newOrchHarness(t, "", nil)
	now := time.Now()
	for i := range orchestratorMaxSpawnsPerHour {
		if !h.daemon.orchestratorSpawnAllowed(now.Add(time.Duration(i) * time.Minute)) {
			t.Fatalf("spawn %d refused under the cap", i+1)
		}
	}
	if h.daemon.orchestratorSpawnAllowed(now.Add(10 * time.Minute)) {
		t.Fatal("a spawn past the hourly cap was allowed")
	}
	if !h.daemon.orchestratorSpawnAllowed(now.Add(61 * time.Minute)) {
		t.Fatal("the cap did not roll over after an hour")
	}
}

// A recycled pane — the orchestrator's pane id now held by another terminal —
// releases the identity, the name and the disable, so the new agent is an
// ordinary member of the herd.
func TestOrchestratorRecycledPaneIsReleased(t *testing.T) {
	h, _, state := newOrchHarness(t, "", nil)
	ctx := context.Background()
	h.daemon.setOrchestratorIdentity(domain.OrchestratorIdentity{PaneID: "wO:p1", TerminalID: "term_a"})
	if err := h.raw.AssignAgentName(ctx, "wO:p1", domain.OrchestratorAgentName); err != nil {
		t.Fatal(err)
	}
	if err := h.raw.SetAgentDisabled(ctx, "wO:p1", true); err != nil {
		t.Fatal(err)
	}
	h.daemon.observeOrchestrator(ctx, []domain.AgentTransition{
		{AgentID: "wO:p1", PaneID: "wO:p1", TerminalID: "term_b", AgentType: "claude", Status: "idle"},
	})
	if h.daemon.orchestratorIdentity().Known() {
		t.Fatal("identity kept for a recycled pane")
	}
	if _, err := os.Stat(filepath.Join(state, orchestratorStateFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("identity file kept: %v", err)
	}
	names, _ := h.raw.AgentNames(ctx)
	if names["wO:p1"] == domain.OrchestratorAgentName {
		t.Error("the new tenant inherited the orchestrator's name")
	}
	if disabled, _ := h.raw.AgentDisabled(ctx, "wO:p1"); disabled {
		t.Error("the new tenant inherited the orchestrator's disable")
	}
}

// Turning the mode off leaves the session alone and keeps ignoring it.
func TestOrchestratorSurvivesTheModeTurningOff(t *testing.T) {
	h, l, _ := newOrchHarness(t, orchestratorOn, nil)
	waitFor(t, 5*time.Second, func() bool { return h.daemon.orchestratorIdentity().Briefed })
	waitOrchestratorIdle(t, h)
	h.daemon.mu.Lock()
	h.daemon.cfg.FullSelfPrompting.Enabled = false
	h.daemon.mu.Unlock()
	agents, _ := h.herdr.ListAgents(context.Background())
	h.daemon.startOrchestratorPass(agents)
	if !h.daemon.isOrchestrator(domain.AgentTransition{PaneID: "wO:p1", TerminalID: "term_orch"}) {
		t.Fatal("the mode turning off made hap stop ignoring the orchestrator")
	}
	if got := h.daemon.withoutOrchestrator(agents); len(got) != len(agents)-1 {
		t.Fatalf("withoutOrchestrator kept it: %d of %d", len(got), len(agents))
	}
	if _, started, _ := l.snapshot(); len(started) != 1 {
		t.Fatalf("starts = %d, want the original one only", len(started))
	}
}

// The identity file is what makes the filter work from the first event after
// a restart.
func TestOrchestratorIdentitySurvivesARestart(t *testing.T) {
	h, _, state := newOrchHarness(t, "", nil)
	h.daemon.setOrchestratorIdentity(domain.OrchestratorIdentity{PaneID: "wO:p7", TerminalID: "term_7"})
	fresh := &Daemon{opt: Options{StateDir: state}}
	fresh.loadOrchestrator()
	if !fresh.isOrchestrator(domain.AgentTransition{PaneID: "wO:p7"}) {
		t.Fatal("a restarted daemon forgot the orchestrator")
	}
	_ = h
}
