package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	skilldoc "github.com/0xGosu/herdr-auto-pilot"
	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/daemonhealth"
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
	closed    []string
	lookups   int
	failStart bool
	// unsupported answers every lookup as a herdr without the verbs does.
	unsupported bool
	// onStart runs as StartAgent begins — the window a real start spends
	// waiting for claude to become ready.
	onStart func()
}

var (
	_ ports.AgentLauncher     = (*orchLauncher)(nil)
	_ ports.VisiblePaneReader = (*orchLauncher)(nil)
)

func (l *orchLauncher) AgentByName(_ context.Context, name string) (domain.AgentTransition, bool, error) {
	l.lmu.Lock()
	defer l.lmu.Unlock()
	l.lookups++
	if l.unsupported {
		return domain.AgentTransition{}, false, ports.ErrLaunchUnsupported
	}
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
	fail, onStart, n := l.failStart, l.onStart, len(l.started)
	l.lmu.Unlock()
	if onStart != nil {
		onStart()
	}
	if fail {
		return errors.New("induced start failure")
	}
	// Each start is a new terminal, as a real one is.
	term := "term_orch"
	if n > 1 {
		term = fmt.Sprintf("term_orch%d", n)
	}
	a := domain.AgentTransition{AgentID: pane, PaneID: pane, WorkspaceID: "wO", AgentType: kind,
		Status: "idle", TerminalID: term}
	l.lmu.Lock()
	l.named[name] = a
	l.lmu.Unlock()
	l.mu.Lock()
	l.agents = append(l.agents, a)
	l.mu.Unlock()
	return nil
}

func (l *orchLauncher) ClosePane(_ context.Context, pane string) error {
	l.lmu.Lock()
	defer l.lmu.Unlock()
	l.closed = append(l.closed, pane)
	return nil
}

// kill makes the named session exit: herdr forgets the name and the agent.
func (l *orchLauncher) kill(name string) {
	l.lmu.Lock()
	a := l.named[name]
	delete(l.named, name)
	l.lmu.Unlock()
	l.mu.Lock()
	defer l.mu.Unlock()
	kept := l.agents[:0]
	for _, b := range l.agents {
		if b.PaneID != a.PaneID {
			kept = append(kept, b)
		}
	}
	l.agents = kept
}

func (l *orchLauncher) closedPanes() []string {
	l.lmu.Lock()
	defer l.lmu.Unlock()
	return append([]string(nil), l.closed...)
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

// orchestratorModeOnIn turns the mode and the key on in the daemon's LIVE
// config — what every step of a pass re-asks — and returns it, for tests that
// drive ensureOrchestrator directly on a harness started with it off.
func orchestratorModeOnIn(h *harness) config.Config {
	// Hold the pass latch first: Run's own startup pass may still be on its
	// way (the harness only waits for the control socket), and one arriving
	// after the mode flips on would race the pass the test drives directly.
	// ensureOrchestrator itself ignores the latch.
	h.daemon.orch.mu.Lock()
	h.daemon.orch.running = true
	h.daemon.orch.mu.Unlock()
	h.daemon.mu.Lock()
	defer h.daemon.mu.Unlock()
	h.daemon.cfg.FullSelfPrompting.Enabled = true
	h.daemon.cfg.FullSelfPrompting.OrchestratorAgentCommand = []string{"claude"}
	return h.daemon.cfg
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
	if strings.Count(brief, "/opt/hap/bin/hap") != 1 || strings.Contains(brief, "{self}") ||
		!strings.Contains(brief, "`hap stream orchestrator`") {
		t.Errorf("brief must name the binary once, as the PATH fallback, and say plain `hap` elsewhere:\n%s", brief)
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
	h, l, state := newOrchHarness(t, orchestratorOn, func(l *orchLauncher) {
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
	// The skills are still installed: the bootstrap runs before the alive/launch
	// branch, so it does not depend on hap having started the session. They land
	// in hap's own directory, which an adopted session may not be running in —
	// the accepted limit named on bootstrapOrchestratorSkills.
	if _, err := os.Stat(filepath.Join(state, orchestratorDirName, ".claude", "skills",
		"hap", skilldoc.SkillFileName)); err != nil {
		t.Errorf("an adopted session's pass installed no skills: %v", err)
	}
}

// An operator's own brief replaces the built-in one, with {self} expanded — and
// a SINGLE-line one is delivered too (herdr routes it as typed keys rather than
// a paste, which the fake records the same way).
func TestOrchestratorSendsTheConfiguredBrief(t *testing.T) {
	h, _, _ := newOrchHarness(t, orchestratorOn+"orchestrator_agent_prompt = \"Run {self} status, then wait.\"\n", nil)
	waitFor(t, 5*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	if got := h.herdr.sentInputs()[0]; got != "Run /opt/hap/bin/hap status, then wait." {
		t.Fatalf("brief sent = %q", got)
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
	t.Run("control: permitted", func(t *testing.T) {
		h, l, _ := newOrchHarness(t, "", nil)
		h.daemon.ensureOrchestrator(context.Background(), l, orchestratorModeOnIn(h), []domain.AgentTransition{})
		if _, started, _ := l.snapshot(); len(started) != 1 {
			t.Fatalf("starts = %d: the refusals below would pass on a pass that never runs", len(started))
		}
	})
	t.Run("paused", func(t *testing.T) {
		h, l, _ := newOrchHarness(t, "", nil)
		on := orchestratorModeOnIn(h)
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
		bad := orchestratorModeOnIn(h)
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
	if closed := l.closedPanes(); len(closed) != 1 || closed[0] != "wO:p1" {
		t.Fatalf("closed panes = %q: a failed start must not leave its tab behind", closed)
	}
	if h.daemon.isOrchestrator(domain.AgentTransition{PaneID: "wO:p1"}) {
		t.Fatal("the pane of a failed start is still ignored")
	}
}

// A herdr without `agent get` stands the feature down quietly for the longest
// backoff, and opens nothing.
func TestOrchestratorUnsupportedHerdrStandsDown(t *testing.T) {
	h, l, _ := newOrchHarness(t, "", func(l *orchLauncher) { l.unsupported = true })
	h.daemon.ensureOrchestrator(context.Background(), l, orchestratorModeOnIn(h), []domain.AgentTransition{})
	if created, _, _ := l.snapshot(); len(created) != 0 {
		t.Fatalf("opened a pane on a herdr that cannot start agents: %q", created)
	}
	h.daemon.orch.mu.Lock()
	retryAt := h.daemon.orch.retryAt
	h.daemon.orch.mu.Unlock()
	if time.Until(retryAt) < orchestratorBackoffMax-time.Minute {
		t.Fatalf("retry at %v: want the longest backoff", retryAt)
	}
}

// A session that keeps dying is re-created at most orchestratorMaxSpawnsPerHour
// times an hour, through the real pass rather than the counter alone.
func TestOrchestratorRespawnsAreCapped(t *testing.T) {
	h, l, _ := newOrchHarness(t, "", nil)
	cfg := orchestratorModeOnIn(h)
	ctx := context.Background()
	for range orchestratorMaxSpawnsPerHour + 2 {
		h.daemon.ensureOrchestrator(ctx, l, cfg, nil)
		l.kill(domain.OrchestratorAgentName)
	}
	if created, started, _ := l.snapshot(); len(started) != orchestratorMaxSpawnsPerHour ||
		len(created) != orchestratorMaxSpawnsPerHour {
		t.Fatalf("panes=%d starts=%d, want exactly %d of each", len(created), len(started), orchestratorMaxSpawnsPerHour)
	}
}

// The mode turned off while `agent start` waited: the session is still
// recorded (so it stays ignored) but nothing is typed into it. The new pane is
// ignored from before the start returns.
func TestOrchestratorModeOffDuringTheStartSendsNoBrief(t *testing.T) {
	h, l, _ := newOrchHarness(t, "", nil)
	cfg := orchestratorModeOnIn(h)
	l.onStart = func() {
		if !h.daemon.isOrchestrator(domain.AgentTransition{PaneID: "wO:p1"}) {
			t.Error("the new pane was not ignored while its session started")
		}
		h.daemon.mu.Lock()
		h.daemon.cfg.FullSelfPrompting.Enabled = false
		h.daemon.mu.Unlock()
	}
	h.daemon.ensureOrchestrator(context.Background(), l, cfg, nil)
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Fatalf("briefed after the mode turned off: %q", got)
	}
	if id := h.daemon.orchestratorIdentity(); id.PaneID != "wO:p1" || id.TerminalID == "" || id.Briefed {
		t.Fatalf("identity = %+v, want the started session recorded and unbriefed", id)
	}
}

// Turning the key on by reload starts the orchestrator without waiting for a
// sweep.
func TestOrchestratorStartsOnTheReloadThatTurnsItOn(t *testing.T) {
	h, l, _ := newOrchHarness(t, "", nil)
	if err := os.WriteFile(h.cfgPath, []byte(orchestratorOn), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.daemon.reload(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool {
		_, started, _ := l.snapshot()
		return len(started) == 1
	})
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

// The pane recycled while the daemon was DOWN: the new tenant carries the old
// orchestrator's name and disable. The ensure pass must release them against
// the old identity BEFORE recording a new one — afterwards nothing compares
// that pane again, and an operator's agent stays disabled for good.
func TestOrchestratorReleasesAPaneRecycledWhileTheDaemonWasDown(t *testing.T) {
	h, l, _ := newOrchHarness(t, "", nil)
	cfg := orchestratorModeOnIn(h)
	ctx := context.Background()
	h.daemon.setOrchestratorIdentity(domain.OrchestratorIdentity{PaneID: "wX:p3", TerminalID: "term_old"})
	if err := h.raw.AssignAgentName(ctx, "wX:p3", domain.OrchestratorAgentName); err != nil {
		t.Fatal(err)
	}
	if err := h.raw.SetAgentDisabled(ctx, "wX:p3", true); err != nil {
		t.Fatal(err)
	}
	l.mu.Lock()
	l.agents = append(l.agents, domain.AgentTransition{AgentID: "wX:p3", PaneID: "wX:p3",
		TerminalID: "term_new", AgentType: "claude", Status: "idle"})
	l.mu.Unlock()

	h.daemon.ensureOrchestrator(ctx, l, cfg, nil)

	names, _ := h.raw.AgentNames(ctx)
	if names["wX:p3"] == domain.OrchestratorAgentName {
		t.Error("the recycled pane kept the orchestrator's name")
	}
	if disabled, _ := h.raw.AgentDisabled(ctx, "wX:p3"); disabled {
		t.Error("the recycled pane stayed disabled")
	}
	if names["wO:p1"] != domain.OrchestratorAgentName {
		t.Errorf("the new orchestrator is named %q", names["wO:p1"])
	}
	if id := h.daemon.orchestratorIdentity(); id.PaneID != "wO:p1" {
		t.Fatalf("identity = %+v", id)
	}
}

// A LIVE agent the operator named "orchestrator" keeps the name — and the new
// session is still disabled, though no ordinary event ever gave its pane a row.
func TestOrchestratorIsDisabledWhenALiveAgentHoldsItsName(t *testing.T) {
	h, l, _ := newOrchHarness(t, "", nil)
	cfg := orchestratorModeOnIn(h)
	ctx := context.Background()
	if err := h.raw.AssignAgentName(ctx, "pZ", domain.OrchestratorAgentName); err != nil {
		t.Fatal(err)
	}
	l.mu.Lock()
	l.agents = append(l.agents, domain.AgentTransition{AgentID: "pZ", PaneID: "pZ",
		TerminalID: "term_z", AgentType: "claude", Status: "idle"})
	l.mu.Unlock()

	h.daemon.ensureOrchestrator(ctx, l, cfg, nil)

	names, _ := h.raw.AgentNames(ctx)
	if names["pZ"] != domain.OrchestratorAgentName {
		t.Errorf("the operator's agent lost its name: %q", names["pZ"])
	}
	if disabled, err := h.raw.AgentDisabled(ctx, "wO:p1"); err != nil || !disabled {
		t.Fatalf("the orchestrator is not disabled: %v (%v)", disabled, err)
	}
	if disabled, _ := h.raw.AgentDisabled(ctx, "pZ"); disabled {
		t.Error("the operator's agent was disabled")
	}
}

// Turning the mode off leaves the session alone and keeps ignoring it — the
// one thing typed into it is the dormant message.
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
	waitOrchestratorIdle(t, h)
	if _, started, _ := l.snapshot(); len(started) != 1 {
		t.Fatalf("starts = %d, want the original one only", len(started))
	}
	if sent := h.herdr.sentInputs(); len(sent) != 2 || !strings.Contains(sent[1], "full self-prompting is now OFF") {
		t.Fatalf("sends = %q, want the brief then the dormant message", sent)
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

// The built-in brief names the binary ONCE (the PATH fallback) and sets up the
// hourly health check that notices a stopped daemon or a hung agent, torn
// down on fsp.off and re-created on fsp.on, and says the stream omits the
// orchestrator's own events.
func TestOrchestratorBriefShape(t *testing.T) {
	if n := strings.Count(orchestratorBrief, "{self}"); n != 1 {
		t.Fatalf("{self} appears %d times, want once", n)
	}
	if n := strings.Count(orchestratorBrief, "{skills}"); n != 1 {
		t.Fatalf("{skills} appears %d times, want once", n)
	}
	for _, want := range []string{
		"`hap stream orchestrator`", "CronCreate", "CronList", "CronDelete",
		"`hap daemon --ensure`", "`fsp.off`", "`fsp.on`",
		// The stream leaves out the orchestrator's own events; a brief that
		// does not say so disagrees with the binary it drives.
		"`--include-self`",
		// The pane check alone attributes only commands run in the
		// orchestrator's own pane; the brief declares the actor explicitly so
		// every command it runs is audited as the orchestrator's.
		"`HAP_ACTOR=orchestrator hap escalations`",
	} {
		if !strings.Contains(orchestratorBrief, want) {
			t.Errorf("the brief does not mention %s", want)
		}
	}
	// The stream prints nothing for the orchestrator's own events — no notice
	// either — so a brief that still describes one sends the session looking
	// for a line that never comes.
	if strings.Contains(orchestratorBrief, "# suppressed") {
		t.Error("the brief still describes a `# suppressed` line the stream no longer prints")
	}
}

// An operator's working directory is where the session starts; one that does
// not exist is never created — the start fails and backs off instead.
func TestOrchestratorUsesTheConfiguredCwd(t *testing.T) {
	h, l, _ := newOrchHarness(t, "", nil)
	cfg := orchestratorModeOnIn(h)
	dir := t.TempDir()
	cfg.FullSelfPrompting.OrchestratorAgentCwd = dir
	h.daemon.ensureOrchestrator(context.Background(), l, cfg, nil)
	if created, _, _ := l.snapshot(); len(created) != 1 || !strings.HasSuffix(created[0], "|"+dir) {
		t.Fatalf("panes created = %q, want one in %s", created, dir)
	}

	h2, l2, _ := newOrchHarness(t, "", nil)
	cfg2 := orchestratorModeOnIn(h2)
	missing := filepath.Join(t.TempDir(), "not-there")
	cfg2.FullSelfPrompting.OrchestratorAgentCwd = missing
	h2.daemon.ensureOrchestrator(context.Background(), l2, cfg2, nil)
	if created, _, _ := l2.snapshot(); len(created) != 0 {
		t.Fatalf("started in a directory that does not exist: %q", created)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the missing working directory was created (%v)", err)
	}
}

// A start that keeps failing, and a brief held by a claude prompt, reach the
// heartbeat — the only way the TUI and `hap status` hear of either — and go
// away once the session is up, or once the feature is switched off.
func TestOrchestratorTroubleReachesTheHeartbeat(t *testing.T) {
	h, l, state := newOrchHarness(t, "", func(l *orchLauncher) { l.failStart = true })
	cfg := orchestratorModeOnIn(h)
	ctx := context.Background()
	if h.daemon.orchestratorHealth() != nil {
		t.Fatal("trouble reported before anything was tried")
	}
	h.daemon.ensureOrchestrator(ctx, l, cfg, nil)
	h.daemon.writeHealth(time.Now())
	rec, ok := daemonhealth.Read(state)
	if !ok || !rec.Orchestrator.Failing() || !strings.Contains(rec.Orchestrator.LastError, "induced start failure") ||
		rec.Orchestrator.RetryAt.IsZero() {
		t.Fatalf("heartbeat orchestrator = %+v, want the start failure with a retry time", rec.Orchestrator)
	}

	// Switched off: nothing to report, whatever happened before.
	h.daemon.mu.Lock()
	h.daemon.cfg.FullSelfPrompting.Enabled = false
	h.daemon.mu.Unlock()
	if o := h.daemon.orchestratorHealth(); o != nil {
		t.Fatalf("a switched-off feature still reports %+v", o)
	}
	h.daemon.mu.Lock()
	h.daemon.cfg.FullSelfPrompting.Enabled = true
	h.daemon.mu.Unlock()

	// It starts, but claude shows a first-run prompt: waiting, not failing.
	l.lmu.Lock()
	l.failStart = false
	l.visible = "Do you trust the files in this folder?\n❯ 1. Yes, proceed\n  2. No, exit\n"
	l.lmu.Unlock()
	h.daemon.ensureOrchestrator(ctx, l, cfg, nil)
	o := h.daemon.orchestratorHealth()
	if o == nil || o.Failing() || !o.Waiting {
		t.Fatalf("orchestrator health = %+v, want waiting and not failing", o)
	}

	// Briefed: all clear.
	l.setVisible(emptyComposer(t))
	h.daemon.ensureOrchestrator(ctx, l, cfg, nil)
	if o := h.daemon.orchestratorHealth(); o != nil {
		t.Fatalf("a briefed orchestrator still reports %+v", o)
	}
}

// The skills go on disk in the session's working directory, so the
// orchestrator can recall them after an automatic compaction has dropped the
// brief that told it to run `hap --skill`.
func TestOrchestratorBootstrapsItsSkillsOnDisk(t *testing.T) {
	h, _, state := newOrchHarness(t, orchestratorOn, nil)
	waitFor(t, 5*time.Second, func() bool { return h.daemon.orchestratorIdentity().Briefed })
	waitOrchestratorIdle(t, h)

	base := filepath.Join(state, orchestratorDirName, ".claude", "skills")
	hap := filepath.Join(base, "hap", skilldoc.SkillFileName)
	got, err := os.ReadFile(hap)
	if err != nil {
		t.Fatalf("read %s: %v", hap, err)
	}
	if string(got) != skilldoc.HapSkill {
		t.Errorf("%s is not the bundled hap skill", hap)
	}
	// The multi-file half: SKILL.md is useless without the references it links.
	entries, err := os.ReadDir(filepath.Join(base, "hap-orchestrator"))
	if err != nil {
		t.Fatalf("read the orchestrator skill dir: %v", err)
	}
	if len(entries) < 2 {
		t.Fatalf("the orchestrator skill installed as %d file(s); it is a tree", len(entries))
	}
	if _, err := os.Stat(filepath.Join(base, "hap-orchestrator", skilldoc.SkillFileName)); err != nil {
		t.Errorf("no orchestrator SKILL.md: %v", err)
	}
}

// The bootstrap sits BEFORE the alive/launch branch, so a session that already
// exists — adopted, or surviving a restart — is brought up to date too. That is
// how an upgraded binary's skills reach an orchestrator nothing re-creates.
func TestOrchestratorRefreshesItsSkillsForALiveSession(t *testing.T) {
	h, l, state := newOrchHarness(t, orchestratorOn, nil)
	waitFor(t, 5*time.Second, func() bool { return h.daemon.orchestratorIdentity().Briefed })
	waitOrchestratorIdle(t, h)

	stale := filepath.Join(state, orchestratorDirName, ".claude", "skills",
		"hap-orchestrator", skilldoc.SkillFileName)
	if err := os.WriteFile(stale, []byte("an older release's skill"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	agents, _ := h.herdr.ListAgents(ctx)
	cfg, _, _ := h.daemon.snapshot()
	h.daemon.ensureOrchestrator(ctx, l, cfg, agents)

	got, err := os.ReadFile(stale)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) == "an older release's skill" {
		t.Error("a live session's stale skill was left as it was")
	}
	// Nothing else happened: the session was healthy.
	if _, started, _ := l.snapshot(); len(started) != 1 || len(h.herdr.sentInputs()) != 1 {
		t.Errorf("the refresh pass restarted or re-briefed it: starts=%d sends=%d",
			len(started), len(h.herdr.sentInputs()))
	}
}

// An operator's orchestrator_agent_cwd is theirs, not hap's: an unattended
// write there could overwrite a project skill of their own. hap writes nothing
// and says so once, naming `hap skill install`.
func TestOrchestratorWritesNoSkillsIntoAnOperatorsCwd(t *testing.T) {
	h, l, state := newOrchHarness(t, "", nil)
	cfg := orchestratorModeOnIn(h)
	dir := t.TempDir()
	cfg.FullSelfPrompting.OrchestratorAgentCwd = dir
	h.daemon.ensureOrchestrator(context.Background(), l, cfg, nil)

	if created, _, _ := l.snapshot(); len(created) != 1 {
		t.Fatalf("the session was not started in %s: %q", dir, created)
	}
	for _, unwanted := range []string{".claude", ".agents"} {
		if _, err := os.Stat(filepath.Join(dir, unwanted)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("hap wrote %s into the operator's working directory (stat err = %v)", unwanted, err)
		}
	}
	if _, err := os.Stat(filepath.Join(state, orchestratorDirName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("hap fell back to its own directory instead of skipping (stat err = %v)", err)
	}
}

// The brief's step 1 must MATCH what the bootstrap did. In hap's own directory
// the skills are there and the session is told to re-read them after a
// compaction; in an operator's orchestrator_agent_cwd nothing was installed, so
// claiming otherwise would send it after a file it can never find —
// `hap --skill` prints only the hap document, never the orchestrator one.
func TestOrchestratorBriefMatchesWhereTheSkillsWent(t *testing.T) {
	h, _, _ := newOrchHarness(t, "", nil)
	cfg := orchestratorModeOnIn(h)

	own := h.daemon.orchestratorPrompt(cfg)
	if !strings.Contains(own, "hap has installed it") || strings.Contains(own, "{skills}") {
		t.Errorf("hap's own directory should promise the installed skills:\n%s", own)
	}

	cfg.FullSelfPrompting.OrchestratorAgentCwd = t.TempDir()
	operators := h.daemon.orchestratorPrompt(cfg)
	if strings.Contains(operators, "hap has installed") || strings.Contains(operators, "{skills}") {
		t.Errorf("an operator's directory has no skills in it; the brief must not claim any:\n%s", operators)
	}
	if !strings.Contains(operators, "`hap --skill`") || !strings.Contains(operators, "installed no skills") {
		t.Errorf("the brief must name the route that DOES work, and say nothing was installed:\n%s", operators)
	}
	// Both variants still tell it how to survive losing this brief.
	for _, text := range []string{own, operators} {
		if !strings.Contains(text, "compacted") {
			t.Errorf("the brief says nothing about a compaction:\n%s", text)
		}
	}
}

// An operator's own brief gets both placeholders too.
func TestOrchestratorCustomBriefExpandsSkills(t *testing.T) {
	h, _, _ := newOrchHarness(t, "", nil)
	cfg := orchestratorModeOnIn(h)
	cfg.FullSelfPrompting.OrchestratorAgentPrompt = "Use {self}. Setup: {skills}"
	got := h.daemon.orchestratorPrompt(cfg)
	if strings.Contains(got, "{skills}") || !strings.Contains(got, "/opt/hap/bin/hap") {
		t.Errorf("a custom brief did not get both placeholders: %s", got)
	}
}

// briefedOrchestrator is a harness whose orchestrator hap started and briefed
// itself — the only kind the dormancy messages go to.
func briefedOrchestrator(t *testing.T) (*harness, *orchLauncher, string) {
	t.Helper()
	h, l, state := newOrchHarness(t, orchestratorOn, nil)
	waitFor(t, 5*time.Second, func() bool { return h.daemon.orchestratorIdentity().HapBriefed() })
	waitOrchestratorIdle(t, h)
	if n := len(h.herdr.sentInputs()); n != 1 {
		t.Fatalf("sends after the brief = %d, want 1", n)
	}
	return h, l, state
}

func setOrchestratorFSP(h *harness, on bool) {
	h.daemon.mu.Lock()
	defer h.daemon.mu.Unlock()
	h.daemon.cfg.FullSelfPrompting.Enabled = on
}

// orchestratorSweep is the minute sweep's orchestrator step, waited out.
func orchestratorSweep(t *testing.T, h *harness) {
	t.Helper()
	agents, err := h.herdr.ListAgents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	h.daemon.startOrchestratorPass(agents)
	waitOrchestratorIdle(t, h)
}

func setKillSwitch(t *testing.T, h *harness, state string) {
	t.Helper()
	if _, err := h.raw.InsertKillEvent(context.Background(), domain.KillEvent{State: state,
		Scope: domain.KillScopeGlobal, Author: "operator", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
}

const (
	dormantMarker = domain.OrchestratorDormantMarker
	wakeMarker    = domain.OrchestratorWakeMarker
)

// The whole cycle: an awake session is left alone, the mode going off sends
// the dormant message exactly once (persisted, so a restart remembers it),
// and the mode coming back sends the wake exactly once.
func TestOrchestratorSleepsAndWakesWithTheMode(t *testing.T) {
	h, _, state := briefedOrchestrator(t)

	// Control: awake with the mode on, nothing is owed.
	orchestratorSweep(t, h)
	if n := len(h.herdr.sentInputs()); n != 1 {
		t.Fatalf("an awake session with the mode on was sent %d messages after its brief", n-1)
	}

	setOrchestratorFSP(h, false)
	orchestratorSweep(t, h)
	orchestratorSweep(t, h)
	sent := h.herdr.sentInputs()
	if len(sent) != 2 || !strings.Contains(sent[1], dormantMarker) {
		t.Fatalf("sends = %q, want the dormant message once", sent)
	}
	if id := h.daemon.orchestratorIdentity(); !id.Dormant {
		t.Fatalf("identity = %+v, want dormant", id)
	}
	restarted := &Daemon{opt: Options{StateDir: state}}
	restarted.loadOrchestrator()
	if id := restarted.orchestratorIdentity(); !id.Dormant || !id.HapBriefed() {
		t.Fatalf("a restarted daemon read %+v: it would not know a wake is owed", id)
	}

	setOrchestratorFSP(h, true)
	orchestratorSweep(t, h)
	orchestratorSweep(t, h)
	sent = h.herdr.sentInputs()
	if len(sent) != 3 || !strings.Contains(sent[2], wakeMarker) {
		t.Fatalf("sends = %q, want the wake once", sent)
	}
	if strings.Contains(sent[2], "{self}") || strings.Contains(sent[2], "{skills}") ||
		!strings.Contains(sent[2], "/opt/hap/bin/hap") {
		t.Errorf("the wake was not rendered:\n%s", sent[2])
	}
	if id := h.daemon.orchestratorIdentity(); id.Dormant {
		t.Fatalf("identity = %+v, want awake", id)
	}
}

// A reload that turns the mode off sends the dormant message without waiting
// for a sweep — the mirror of TestOrchestratorStartsOnTheReloadThatTurnsItOn.
func TestOrchestratorGoesDormantOnTheReloadThatTurnsItOff(t *testing.T) {
	h, _, _ := briefedOrchestrator(t)
	if err := os.WriteFile(h.cfgPath, []byte(strings.Replace(orchestratorOn, "enabled = true", "enabled = false", 1)),
		0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.daemon.reload(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return h.daemon.orchestratorIdentity().Dormant })
	if sent := h.herdr.sentInputs(); len(sent) != 2 || !strings.Contains(sent[1], dormantMarker) {
		t.Fatalf("sends = %q", sent)
	}
}

// A stand-down at a [limits] ceiling is the mode going off too.
func TestOrchestratorGoesDormantWhenTheModeStandsDown(t *testing.T) {
	h, _, _ := briefedOrchestrator(t)
	h.daemon.mu.Lock()
	h.daemon.fspCeilingLatched = true
	h.daemon.mu.Unlock()
	orchestratorSweep(t, h)
	if sent := h.herdr.sentInputs(); len(sent) != 2 || !strings.Contains(sent[1], dormantMarker) {
		t.Fatalf("sends = %q, want the dormant message", sent)
	}
}

// The kill switch holds the dormant message — hap types into no pane while the
// herd is paused — and the resume releases it. A toggle undone while it was
// held sends nothing at all: the delivered state already matches.
func TestOrchestratorDormancyWaitsForTheKillSwitch(t *testing.T) {
	t.Run("held, then sent on resume", func(t *testing.T) {
		h, _, _ := briefedOrchestrator(t)
		setKillSwitch(t, h, domain.KillStateActiveValue)
		setOrchestratorFSP(h, false)
		orchestratorSweep(t, h)
		if n := len(h.herdr.sentInputs()); n != 1 {
			t.Fatalf("a paused herd's orchestrator was sent %d messages", n-1)
		}
		setKillSwitch(t, h, domain.KillStateResumed)
		orchestratorSweep(t, h)
		if sent := h.herdr.sentInputs(); len(sent) != 2 || !strings.Contains(sent[1], dormantMarker) {
			t.Fatalf("sends after the resume = %q, want the dormant message", sent)
		}
	})
	t.Run("undone while held", func(t *testing.T) {
		h, _, _ := briefedOrchestrator(t)
		setKillSwitch(t, h, domain.KillStateActiveValue)
		setOrchestratorFSP(h, false)
		orchestratorSweep(t, h)
		setOrchestratorFSP(h, true)
		setKillSwitch(t, h, domain.KillStateResumed)
		orchestratorSweep(t, h)
		if n := len(h.herdr.sentInputs()); n != 1 {
			t.Fatalf("an off→on toggle that never delivered sent %d messages: %q", n-1, h.herdr.sentInputs())
		}
	})
	t.Run("wake held too", func(t *testing.T) {
		h, _, _ := briefedOrchestrator(t)
		setOrchestratorFSP(h, false)
		orchestratorSweep(t, h)
		setKillSwitch(t, h, domain.KillStateActiveValue)
		setOrchestratorFSP(h, true)
		orchestratorSweep(t, h)
		if n := len(h.herdr.sentInputs()); n != 2 {
			t.Fatalf("sends = %d, want the brief and the dormant message only", n)
		}
		setKillSwitch(t, h, domain.KillStateResumed)
		orchestratorSweep(t, h)
		if sent := h.herdr.sentInputs(); len(sent) != 3 || !strings.Contains(sent[2], wakeMarker) {
			t.Fatalf("sends after the resume = %q, want the wake", sent)
		}
	})
}

// A composer that is not ready defers the message silently — nothing is typed
// into a modal and, unlike the brief, the operator is not notified — and the
// next pass delivers it.
func TestOrchestratorDormancyWaitsForAReadyComposer(t *testing.T) {
	h, l, _ := briefedOrchestrator(t)
	l.setVisible("Do you trust the files in this folder?\n❯ 1. Yes, proceed\n  2. No, exit\n")
	setOrchestratorFSP(h, false)
	orchestratorSweep(t, h)
	if n := len(h.herdr.sentInputs()); n != 1 {
		t.Fatalf("typed into a modal: %q", h.herdr.sentInputs())
	}
	h.herdr.mu.Lock()
	notified := len(h.herdr.notifications)
	h.herdr.mu.Unlock()
	if notified != 0 {
		t.Fatalf("notifications = %d, want none", notified)
	}
	l.setVisible(emptyComposer(t))
	orchestratorSweep(t, h)
	if sent := h.herdr.sentInputs(); len(sent) != 2 || !strings.Contains(sent[1], dormantMarker) {
		t.Fatalf("sends = %q, want the dormant message once the composer is ready", sent)
	}
}

// An adopted session is never typed into: neither message goes to it.
func TestOrchestratorDormancySkipsAnAdoptedSession(t *testing.T) {
	existing := domain.AgentTransition{AgentID: "wX:p9", PaneID: "wX:p9", AgentType: "claude",
		Status: "idle", TerminalID: "term_x"}
	h, _, _ := newOrchHarness(t, orchestratorOn, func(l *orchLauncher) {
		l.named[domain.OrchestratorAgentName] = existing
		l.agents = append(l.agents, existing)
	})
	waitFor(t, 5*time.Second, func() bool { return h.daemon.orchestratorIdentity().Known() })
	waitOrchestratorIdle(t, h)
	setOrchestratorFSP(h, false)
	orchestratorSweep(t, h)
	// Even an identity that somehow reads dormant gets no wake.
	id := h.daemon.orchestratorIdentity()
	id.Dormant = true
	h.daemon.updateOrchestratorIfCurrent(id)
	setOrchestratorFSP(h, true)
	orchestratorSweep(t, h)
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Fatalf("an adopted session was typed into: %q", got)
	}
}

// Failed sends are bounded per toggle: three, then nothing until the mode
// flips back. The flip starts a fresh count — and because those failures may
// have landed, the session's state is unknown, so the flip's own message is
// owed even though nothing was ever confirmed delivered.
func TestOrchestratorDormancySendsAreBounded(t *testing.T) {
	h, _, _ := briefedOrchestrator(t)
	h.herdr.setFailSend(true)
	setOrchestratorFSP(h, false)
	for range orchestratorNudgeAttempts + 2 {
		orchestratorSweep(t, h)
	}
	id := h.daemon.orchestratorIdentity()
	if id.Dormant || !id.NudgeUnconfirmed || id.NudgeAttempts != orchestratorNudgeAttempts {
		t.Fatalf("identity = %+v, want %d unconfirmed attempts and not confirmed dormant", id, orchestratorNudgeAttempts)
	}
	if h.daemon.orchestratorHealth() != nil {
		t.Error("a dormant-message failure reached the heartbeat, where it would surface stale once the mode is back")
	}
	h.herdr.setFailSend(false)
	setOrchestratorFSP(h, true)
	orchestratorSweep(t, h)
	sent := h.herdr.sentInputs()
	if len(sent) != 2 || !strings.Contains(sent[1], wakeMarker) {
		t.Fatalf("sends = %q, want a wake: the failed dormant sends may have landed", sent)
	}
	if id := h.daemon.orchestratorIdentity(); id.Dormant || id.NudgeUnconfirmed || id.NudgeAttempts != 0 {
		t.Fatalf("identity = %+v, want confirmed awake with no spent attempts", id)
	}
	setOrchestratorFSP(h, false)
	orchestratorSweep(t, h)
	if sent := h.herdr.sentInputs(); len(sent) != 3 || !strings.Contains(sent[2], dormantMarker) {
		t.Fatalf("sends = %q, want the next toggle's dormant message", sent)
	}
}

// A dormant send herdr reported failed may still have put the session to
// sleep: the mode coming back before any retry must still wake it. The
// control: a toggle with no send at all owes nothing (see "undone while held").
func TestOrchestratorWakesAfterADormantSendThatMayHaveLanded(t *testing.T) {
	h, _, _ := briefedOrchestrator(t)
	h.herdr.setFailSend(true)
	setOrchestratorFSP(h, false)
	orchestratorSweep(t, h)
	h.herdr.setFailSend(false)
	setOrchestratorFSP(h, true)
	orchestratorSweep(t, h)
	if sent := h.herdr.sentInputs(); len(sent) != 2 || !strings.Contains(sent[1], wakeMarker) {
		t.Fatalf("sends = %q, want the wake", sent)
	}
}

// A failed wake surfaces on the heartbeat, and the next toggle clears it
// rather than leaving it to describe a state that no longer holds.
func TestOrchestratorWakeFailureReachesTheHeartbeatAndClears(t *testing.T) {
	h, _, _ := briefedOrchestrator(t)
	setOrchestratorFSP(h, false)
	orchestratorSweep(t, h)
	h.herdr.setFailSend(true)
	setOrchestratorFSP(h, true)
	orchestratorSweep(t, h)
	if hl := h.daemon.orchestratorHealth(); hl == nil || !strings.Contains(hl.LastError, "wake") {
		t.Fatalf("health = %+v, want the wake failure", hl)
	}
	// Sends still failing: a delivered dormant message would clear the error
	// itself, so only the toggle's settle can be what clears it here.
	setOrchestratorFSP(h, false)
	orchestratorSweep(t, h)
	setOrchestratorFSP(h, true)
	if hl := h.daemon.orchestratorHealth(); hl != nil {
		t.Fatalf("health = %+v: the old wake failure outlived its toggle", hl)
	}
}

// What a restarted daemon does: an identity recorded dormant, a live session,
// the mode on, and Run's startup pass (a nil listing) — the wake and nothing
// else: no lookup, no start, no second brief.
func TestOrchestratorWakesADormantSessionFromTheStartupPass(t *testing.T) {
	h, l, _ := newOrchHarness(t, "", nil)
	live := domain.AgentTransition{AgentID: "wO:p4", PaneID: "wO:p4", AgentType: "claude",
		Status: "idle", TerminalID: "term_4"}
	l.mu.Lock()
	l.agents = append(l.agents, live)
	l.mu.Unlock()
	h.daemon.setOrchestratorIdentity(domain.OrchestratorIdentity{PaneID: "wO:p4", TerminalID: "term_4",
		Briefed: true, BriefAttempts: 1, Dormant: true})
	setOrchestratorFSP(h, true)
	h.daemon.mu.Lock()
	h.daemon.cfg.FullSelfPrompting.OrchestratorAgentCommand = []string{"claude"}
	h.daemon.mu.Unlock()
	h.daemon.startOrchestratorPass(nil)
	waitOrchestratorIdle(t, h)
	sent := h.herdr.sentInputs()
	if len(sent) != 1 || !strings.Contains(sent[0], wakeMarker) {
		t.Fatalf("sends = %q, want exactly the wake", sent)
	}
	if created, started, lookups := l.snapshot(); len(created) != 0 || len(started) != 0 || lookups != 0 {
		t.Fatalf("a live dormant session was looked up or replaced: created=%q started=%q lookups=%d",
			created, started, lookups)
	}
}

// The mode-off path never creates: with the session gone, neither a sweep
// listing nor a nil one looks up, starts or installs anything.
func TestOrchestratorModeOffPassCreatesNothing(t *testing.T) {
	h, l, state := newOrchHarness(t, "", nil)
	h.daemon.setOrchestratorIdentity(domain.OrchestratorIdentity{PaneID: "wO:gone", TerminalID: "term_gone",
		Briefed: true, BriefAttempts: 1})
	orchestratorSweep(t, h)
	h.daemon.startOrchestratorPass(nil)
	waitOrchestratorIdle(t, h)
	if created, started, lookups := l.snapshot(); len(created) != 0 || len(started) != 0 || lookups != 0 {
		t.Fatalf("the mode-off pass created: created=%q started=%q lookups=%d", created, started, lookups)
	}
	if _, err := os.Stat(filepath.Join(state, orchestratorDirName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the mode-off pass installed skills (stat: %v)", err)
	}
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Fatalf("sends = %q", got)
	}
}

// Both messages carry what a session with nothing else to go on needs: the
// tools to stand down and re-arm, the HAP_ACTOR prefix, the PATH fallback, and
// the wake's fresh (non-resumed) Monitor.
func TestOrchestratorDormancyMessageShape(t *testing.T) {
	for _, want := range []string{"TaskStop", "CronDelete", "CronList", "`hap stream orchestrator`", dormantMarker} {
		if !strings.Contains(orchestratorDormantNudge, want) {
			t.Errorf("the dormant message does not mention %s", want)
		}
	}
	for _, want := range []string{"CronCreate", "CronList", "TaskStop", "`hap stream orchestrator`", "without `--resume`",
		"`HAP_ACTOR=orchestrator`", "{self}", "{skills}", "`hap escalations`", wakeMarker} {
		if !strings.Contains(orchestratorWakeNudge, want) {
			t.Errorf("the wake message does not mention %s", want)
		}
	}
}

// reclaimHarness is a herd with the mode off and a live claude session named
// "orchestrator" at wO:p8 / term_8 whose screen is visible — and nothing else:
// the test sets the identity hap remembers (if any), then turns the mode on.
func reclaimHarness(t *testing.T, visible string) (*harness, *orchLauncher) {
	t.Helper()
	h, l, _ := newOrchHarness(t, "", nil)
	live := domain.AgentTransition{AgentID: "wO:p8", PaneID: "wO:p8", AgentType: "claude",
		Status: "idle", TerminalID: "term_8"}
	l.lmu.Lock()
	l.named[domain.OrchestratorAgentName] = live
	l.lmu.Unlock()
	l.mu.Lock()
	l.agents = append(l.agents, live)
	l.mu.Unlock()
	l.setVisible(visible)
	return h, l
}

// turnOrchestratorOnAndPass turns the mode on and runs the startup pass (a nil
// listing), the way a daemon restarted with the mode on does.
func turnOrchestratorOnAndPass(t *testing.T, h *harness) {
	t.Helper()
	h.daemon.mu.Lock()
	h.daemon.cfg.FullSelfPrompting.Enabled = true
	h.daemon.cfg.FullSelfPrompting.OrchestratorAgentCommand = []string{"claude"}
	h.daemon.mu.Unlock()
	h.daemon.startOrchestratorPass(nil)
	waitOrchestratorIdle(t, h)
}

// dormantScreen is the orchestrator's screen after hap put it to sleep: the
// dormant message (wrapped, as claude shows it), its one-line reply, and the
// empty composer it has sat at since.
func dormantScreen(t *testing.T) string {
	t.Helper()
	return "> " + domain.OrchestratorDormantMarker[:30] + "\n  " + domain.OrchestratorDormantMarker[30:] +
		" on this machine, so the herd needs\n  nothing from you until hap needs you again.\n⏺ Dormant.\n\n" +
		emptyComposer(t)
}

// hap lost its record of the session it put to sleep (the identity file gone
// or unreadable), so it finds that session again by name. Its screen proves hap
// was driving it — its own dormant message, never followed by a wake — so it
// is reclaimed and woken, not adopted and left asleep for as long as the mode
// stays on. Controls: a session whose screen shows no dormant message, or one
// already woken since, is adopted as before and never typed into.
func TestOrchestratorReclaimsItsDormantSessionByItsScreen(t *testing.T) {
	h, l := reclaimHarness(t, dormantScreen(t))
	turnOrchestratorOnAndPass(t, h)
	sent := h.herdr.sentInputs()
	if len(sent) != 1 || !strings.Contains(sent[0], wakeMarker) {
		t.Fatalf("sends = %q, want exactly the wake", sent)
	}
	if _, started, _ := l.snapshot(); len(started) != 0 {
		t.Fatalf("a second orchestrator was started beside the sleeping one: %q", started)
	}
	if id := h.daemon.orchestratorIdentity(); !id.HapBriefed() || id.Dormant {
		t.Fatalf("identity = %+v, want hap's own session, awake", id)
	}

	for name, screen := range map[string]string{
		"no dormant message": emptyComposer(t),
		"already woken": dormantScreen(t) + "\n> " + domain.OrchestratorWakeMarker + ". Wake up\n" +
			emptyComposer(t),
	} {
		h, _ := reclaimHarness(t, screen)
		turnOrchestratorOnAndPass(t, h)
		if got := h.herdr.sentInputs(); len(got) != 0 {
			t.Errorf("%s: an adopted session was typed into: %q", name, got)
		}
		if id := h.daemon.orchestratorIdentity(); id.HapBriefed() {
			t.Errorf("%s: identity = %+v, want an ordinary adoption", name, id)
		}
	}
}

// hap still holds its record, but the session's pane id changed (the pane was
// moved, or the listing renumbered it), so the record no longer finds it and
// hap finds it by name. The terminal id is the same session's, so the record —
// briefed by hap, asleep — carries over and the wake goes out, whatever the
// screen shows. Control: a different terminal under the name is somebody
// else's session and is adopted as before.
func TestOrchestratorReclaimsItsMovedSessionByItsTerminal(t *testing.T) {
	for _, tc := range []struct {
		name     string
		terminal string
		wantWake bool
	}{
		{"same terminal", "term_8", true},
		{"another terminal", "term_other", false},
	} {
		h, _ := reclaimHarness(t, emptyComposer(t))
		h.daemon.setOrchestratorIdentity(domain.OrchestratorIdentity{PaneID: "wO:p1", TerminalID: tc.terminal,
			Briefed: true, BriefAttempts: 1, Dormant: true, NudgeTarget: true})
		turnOrchestratorOnAndPass(t, h)
		sent := h.herdr.sentInputs()
		if tc.wantWake {
			if len(sent) != 1 || !strings.Contains(sent[0], wakeMarker) {
				t.Errorf("%s: sends = %q, want exactly the wake", tc.name, sent)
			}
			if id := h.daemon.orchestratorIdentity(); id.PaneID != "wO:p8" || !id.HapBriefed() || id.Dormant {
				t.Errorf("%s: identity = %+v, want hap's own session at its new pane, awake", tc.name, id)
			}
			continue
		}
		if len(sent) != 0 {
			t.Errorf("%s: somebody else's session was typed into: %q", tc.name, sent)
		}
	}
}
