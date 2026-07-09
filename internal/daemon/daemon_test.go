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

	"github.com/0xGosu/herdr-auto-pilot/internal/classify"
	"github.com/0xGosu/herdr-auto-pilot/internal/control"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
	"github.com/0xGosu/herdr-auto-pilot/internal/testutil"
)

// --- fakes ---

type fakeHerdr struct {
	mu            sync.Mutex
	pane          string
	sent          []string
	notifications []string
	failSend      bool
	panicOnRead   bool
	failRead      bool
}

func (f *fakeHerdr) Send(ctx context.Context, paneID, input string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failSend {
		return errors.New("induced send failure")
	}
	f.sent = append(f.sent, input)
	return nil
}

func (f *fakeHerdr) ReadPane(ctx context.Context, paneID string, lines int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.panicOnRead {
		panic("induced pane read panic")
	}
	if f.failRead {
		return "", errors.New("induced read failure")
	}
	return f.pane, nil
}

func (f *fakeHerdr) ListAgents(ctx context.Context) ([]domain.AgentTransition, error) {
	return nil, nil
}

func (f *fakeHerdr) Notify(ctx context.Context, title, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notifications = append(f.notifications, title)
	return nil
}

func (f *fakeHerdr) setPane(content string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pane = content
}

func (f *fakeHerdr) sentInputs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

func (f *fakeHerdr) notified() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.notifications...)
}

type fakeEvents struct {
	ch chan domain.AgentTransition
}

func (f *fakeEvents) Subscribe(ctx context.Context, out chan<- domain.AgentTransition) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case tr := <-f.ch:
			out <- tr
		}
	}
}

type fakeLLM struct {
	configured bool
	consult    func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error)
}

func (f *fakeLLM) Configured() bool { return f.configured }
func (f *fakeLLM) Consult(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
	if f.consult == nil {
		return nil, errors.New("no consult configured")
	}
	return f.consult(ctx, req)
}

// failingStore injects persistence failures on audit writes (FR-024).
type failingStore struct {
	ports.StorePort
	mu        sync.Mutex
	failAudit bool
}

func (f *failingStore) setFailAudit(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failAudit = v
}

func (f *failingStore) AppendAudit(ctx context.Context, a domain.AuditRecord) (int64, error) {
	f.mu.Lock()
	fail := f.failAudit
	f.mu.Unlock()
	if fail {
		return 0, errors.New("induced audit write failure")
	}
	return f.StorePort.AppendAudit(ctx, a)
}

// --- harness ---

type harness struct {
	t       *testing.T
	daemon  *Daemon
	store   ports.StorePort
	raw     *store.Store
	herdr   *fakeHerdr
	events  *fakeEvents
	llm     *fakeLLM
	cfgPath string
	ctlPath string
	cancel  context.CancelFunc
}

func newHarness(t *testing.T, cfgTOML string) *harness {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if cfgTOML != "" {
		if err := os.WriteFile(cfgPath, []byte(cfgTOML), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })

	fs := &failingStore{StorePort: raw}
	fh := &fakeHerdr{}
	fe := &fakeEvents{ch: make(chan domain.AgentTransition, 64)}
	fl := &fakeLLM{}

	// Socket paths must stay short for macOS (104-byte cap).
	ctlPath := filepath.Join(testutil.SocketDir(t), "control.sock")
	d, err := New(Options{
		ConfigPath:        cfgPath,
		ControlSocketPath: ctlPath,
		Store:             fs,
		Herdr:             fh,
		Events:            fe,
		Notify:            fh,
		LLM:               fl,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go d.Run(ctx)
	// Give the control socket a moment to come up.
	waitFor(t, time.Second, func() bool {
		_, err := os.Stat(ctlPath)
		return err == nil
	})

	return &harness{
		t: t, daemon: d, store: fs, raw: raw, herdr: fh, events: fe, llm: fl,
		cfgPath: cfgPath, ctlPath: ctlPath, cancel: cancel,
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

func (h *harness) push(agentID, status string) {
	h.events.ch <- domain.AgentTransition{
		AgentID: agentID, PaneID: agentID, AgentType: "claude", Status: status,
	}
}

// seedAutonomous trains a signature to autonomous mode with a consistent
// history, mirroring graduated shadow-mode learning.
func (h *harness) seedAutonomous(pane string, situationType domain.SituationType, action string) string {
	h.t.Helper()
	ctx := context.Background()
	// Classify exactly as the live pipeline will, so the seeded signature
	// matches the runtime signature (option sets, permission verbs, and
	// error summaries all feed the hash).
	status := "blocked"
	if situationType == domain.SituationIdle {
		status = "idle"
	}
	s := classifierForTest().Classify("claude", status, pane)
	if s.Type != situationType {
		h.t.Fatalf("fixture classifies as %v, expected %v", s.Type, situationType)
	}
	sig := domain.ComputeSignature(s)
	if sig.Verdict != domain.GuardOK {
		h.t.Fatalf("seed situation over-masked: %q", sig.Salient)
	}
	for i := 0; i < 8; i++ {
		if _, err := h.raw.RecordDecision(ctx, domain.DecisionRecord{
			Signature: sig.Signature, SituationType: situationType, AgentType: "claude",
			ChosenAction: action, Source: domain.SourceOperator,
			CreatedAt: time.Now().Add(-time.Duration(8-i) * time.Minute),
		}); err != nil {
			h.t.Fatal(err)
		}
	}
	if err := h.raw.UpsertSignature(context.Background(), domain.SignatureState{
		Signature: sig.Signature, SituationType: situationType, AgentType: "claude",
		Mode: domain.ModeAutonomous, ConsecutiveConfirmations: 8,
		CachedConfidence: 1.0, UpdatedAt: time.Now(),
	}); err != nil {
		h.t.Fatal(err)
	}
	return sig.Signature
}

// classifierForTest returns the same default classifier the daemon uses, so
// seeded signatures match live pipeline signatures exactly.
func classifierForTest() *classify.Classifier {
	return classify.New(nil)
}

// --- tests ---

const approvalPane = "Bash(go test ./...)\n\nDo you want to proceed?\n❯ 1. Yes\n  2. No, and tell the agent what to do differently\n"

func TestPipelineAutoApprovesConfidentSignature(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setPane(approvalPane)
	h.seedAutonomous(approvalPane, domain.SituationApproval, "1")

	h.push("agent-1", "blocked")

	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	if got := h.herdr.sentInputs()[0]; got != "1" {
		t.Errorf("sent %q, want learned approval \"1\"", got)
	}

	// FR-020/NFR-005: the action carries an audit record.
	audits, err := h.raw.AuditLog(context.Background(), 10)
	if err != nil || len(audits) == 0 {
		t.Fatalf("audit log: %v %v", audits, err)
	}
	if audits[0].Status != "auto" || audits[0].Input != "1" {
		t.Errorf("audit record mismatch: %+v", audits[0])
	}
}

func TestPipelineShadowModeEscalatesWithSuggestion(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setPane(approvalPane)
	// History exists but the signature never graduated.
	ctx := context.Background()
	s := classifierForTest().Classify("claude", "blocked", approvalPane)
	sig := domain.ComputeSignature(s)
	for i := 0; i < 3; i++ {
		h.raw.RecordDecision(ctx, domain.DecisionRecord{
			Signature: sig.Signature, SituationType: domain.SituationApproval,
			AgentType: "claude", ChosenAction: "1", Source: domain.SourceOperator,
			CreatedAt: time.Now(),
		})
	}

	h.push("agent-2", "blocked")

	waitFor(t, 3*time.Second, func() bool {
		esc, _ := h.raw.PendingEscalations(ctx)
		return len(esc) == 1
	})
	if len(h.herdr.sentInputs()) != 0 {
		t.Fatal("shadow mode must not send input")
	}
	esc, _ := h.raw.PendingEscalations(ctx)
	if esc[0].Suggestion == "" {
		t.Error("shadow escalation should carry a suggestion")
	}
	if len(h.herdr.notified()) == 0 {
		t.Error("escalation should raise a notification (IR-003)")
	}
}

func TestKillSwitchHonoredWithoutNudge(t *testing.T) {
	// FR-017 / SC-2: the daemon reads the latest kill row every tick, so a
	// kill written directly to the DB (no nudge) still halts automation.
	h := newHarness(t, "")
	h.herdr.setPane(approvalPane)
	h.seedAutonomous(approvalPane, domain.SituationApproval, "1")

	_, err := h.raw.InsertKillEvent(context.Background(), domain.KillEvent{
		State: "active", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}

	h.push("agent-3", "blocked")

	waitFor(t, 3*time.Second, func() bool {
		esc, _ := h.raw.PendingEscalations(context.Background())
		return len(esc) == 1
	})
	if len(h.herdr.sentInputs()) != 0 {
		t.Fatal("kill switch must block all automated action")
	}

	// Resume re-enables automation.
	h.raw.InsertKillEvent(context.Background(), domain.KillEvent{State: "resumed", CreatedAt: time.Now()})
	h.push("agent-3", "blocked")
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
}

func TestAllowlistBlocksDestructiveApproval(t *testing.T) {
	// FR-015 safety invariant end to end: allowlist match escalates even
	// for a fully trained autonomous signature.
	pane := "Do you want to proceed?\nBash(git push --force origin main)\n❯ 1. Yes\n  2. No\n"
	h := newHarness(t, "")
	h.herdr.setPane(pane)
	h.seedAutonomous(pane, domain.SituationApproval, "1")

	h.push("agent-4", "blocked")

	waitFor(t, 3*time.Second, func() bool {
		esc, _ := h.raw.PendingEscalations(context.Background())
		return len(esc) == 1
	})
	if len(h.herdr.sentInputs()) != 0 {
		t.Fatal("allowlist-matched operation must never be auto-executed")
	}
	esc, _ := h.raw.PendingEscalations(context.Background())
	if !contains(esc[0].Rationale, "allowlist") {
		t.Errorf("escalation should cite the allowlist, got %q", esc[0].Rationale)
	}
}

func TestPersistenceFailureBlocksAction(t *testing.T) {
	// FR-024: a simulated audit write failure blocks the autonomous action
	// — no input is sent — and raises an operator-visible notification.
	h := newHarness(t, "")
	h.herdr.setPane(approvalPane)
	h.seedAutonomous(approvalPane, domain.SituationApproval, "1")

	h.store.(*failingStore).setFailAudit(true)
	h.push("agent-5", "blocked")

	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.notified()) >= 1 })
	if len(h.herdr.sentInputs()) != 0 {
		t.Fatal("no action may occur without a durably committed audit record")
	}
	if !contains(h.herdr.notified()[0], "persistence") {
		t.Errorf("notification should cite persistence failure, got %v", h.herdr.notified())
	}

	// Recovery: once persistence works again, the same situation acts.
	h.store.(*failingStore).setFailAudit(false)
	h.push("agent-5", "blocked")
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
}

func TestPaneReadFailureTakesNoAction(t *testing.T) {
	// FR-023: Herdr unreachable → no automated action, condition logged.
	h := newHarness(t, "")
	h.herdr.failRead = true
	h.push("agent-6", "blocked")

	waitFor(t, 3*time.Second, func() bool {
		audits, _ := h.raw.AuditLog(context.Background(), 10)
		return len(audits) == 1
	})
	if len(h.herdr.sentInputs()) != 0 {
		t.Fatal("no input may be sent while Herdr pane reads fail")
	}
	audits, _ := h.raw.AuditLog(context.Background(), 10)
	if !contains(audits[0].Rationale, string(domain.ReasonHerdrUnreachable)) {
		t.Errorf("audit should record herdr_unreachable, got %+v", audits[0])
	}
}

func TestPanicInjectionAtAdapterBoundaries(t *testing.T) {
	// SC-4: a panic at an adapter boundary is caught by the daemon guard —
	// the daemon keeps running and processes subsequent events.
	h := newHarness(t, "")
	h.herdr.panicOnRead = true
	h.push("agent-7", "blocked")
	time.Sleep(200 * time.Millisecond)

	// Daemon survived: turn the panic off and process a normal event.
	h.herdr.mu.Lock()
	h.herdr.panicOnRead = false
	h.herdr.pane = approvalPane
	h.herdr.mu.Unlock()
	h.seedAutonomous(approvalPane, domain.SituationApproval, "1")

	h.push("agent-7", "blocked")
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
}

func TestRunawayGuardPausesAgent(t *testing.T) {
	// FR-019 end to end: the 6th consecutive auto-prompt is blocked and the
	// agent pauses until human interaction.
	h := newHarness(t, "")
	h.herdr.setPane(approvalPane)
	h.seedAutonomous(approvalPane, domain.SituationApproval, "1")

	for i := 0; i < 5; i++ {
		h.push("agent-8", "blocked")
		want := i + 1
		waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == want })
	}
	// 6th: blocked + escalated.
	h.push("agent-8", "blocked")
	waitFor(t, 3*time.Second, func() bool {
		esc, _ := h.raw.PendingEscalations(context.Background())
		return len(esc) == 1
	})
	if len(h.herdr.sentInputs()) != 5 {
		t.Fatalf("6th consecutive prompt must be blocked; sent %d", len(h.herdr.sentInputs()))
	}
	rate, _ := h.raw.GetAgentRate(context.Background(), "agent-8")
	if !rate.Paused {
		t.Error("agent should be paused pending human check-in")
	}

	// Human interaction (agent starts working without our input) resumes.
	h.push("agent-8", "working")
	waitFor(t, 3*time.Second, func() bool {
		r, _ := h.raw.GetAgentRate(context.Background(), "agent-8")
		return !r.Paused && r.ConsecutiveAuto == 0
	})
}

func TestIdleDeclaredTaskSourceDrivesNextPrompt(t *testing.T) {
	// FR-011 tier 1: operator-declared task list → next unchecked item.
	dir := t.TempDir()
	taskFile := filepath.Join(dir, "tasks.md")
	os.WriteFile(taskFile, []byte("- [x] step one\n- [ ] step two\n- [ ] step three\n"), 0o600)

	idlePane := "All tests pass. Task is complete.\n"
	cfg := fmt.Sprintf("[[task_sources]]\nagent = \"agent-9\"\npath = %q\n", taskFile)
	h := newHarness(t, cfg)
	h.herdr.setPane(idlePane)
	h.seedAutonomous(idlePane, domain.SituationIdle, domain.ActionNextDeclaredTask)

	h.push("agent-9", "idle")
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	if got := h.herdr.sentInputs()[0]; got != "step two" {
		t.Errorf("sent %q, want next unchecked item \"step two\"", got)
	}
}

func TestIdleWithoutTaskSourceEscalates(t *testing.T) {
	// FR-011: no declared source, no structured signal → escalate, never a
	// synthesized "continue".
	idlePane := "Task is complete. I could also look into performance sometime.\n"
	h := newHarness(t, "")
	h.herdr.setPane(idlePane)
	h.seedAutonomous(idlePane, domain.SituationIdle, domain.ActionNextDeclaredTask)

	h.push("agent-10", "idle")
	waitFor(t, 3*time.Second, func() bool {
		esc, _ := h.raw.PendingEscalations(context.Background())
		return len(esc) == 1
	})
	if len(h.herdr.sentInputs()) != 0 {
		t.Fatal("no arbitrary prompt may be synthesized")
	}
}

func TestErrorRetryCeilingEndToEnd(t *testing.T) {
	// FR-014: up to 2 automated retries per error signature; 3rd escalates.
	errorPane := "ERROR: build failed with exit status 2\nRetry, skip, or abort?\n"
	h := newHarness(t, "")
	h.herdr.setPane(errorPane)
	h.seedAutonomous(errorPane, domain.SituationError, "retry")

	for i := 0; i < 2; i++ {
		h.push("agent-11", "blocked")
		want := i + 1
		waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == want })
	}
	h.push("agent-11", "blocked")
	waitFor(t, 3*time.Second, func() bool {
		esc, _ := h.raw.PendingEscalations(context.Background())
		return len(esc) == 1
	})
	if len(h.herdr.sentInputs()) != 2 {
		t.Fatalf("3rd error occurrence must escalate; sent %d", len(h.herdr.sentInputs()))
	}
}

func TestCorrectionDemotesAutonomousSignature(t *testing.T) {
	// FR-007 + FR-021 via the control socket: a front-end-written
	// correction demotes the signature on reload.
	h := newHarness(t, "")
	h.herdr.setPane(approvalPane)
	sigKey := h.seedAutonomous(approvalPane, domain.SituationApproval, "1")

	h.push("agent-12", "blocked")
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })

	ctx := context.Background()
	audits, _ := h.raw.AuditLog(ctx, 1)
	if _, err := h.raw.InsertCorrection(ctx, domain.CorrectionRecord{
		AuditID: audits[0].ID, CorrectedAction: "2", Author: "operator", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := control.Nudge(ctx, h.ctlPath, control.KindReload); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 3*time.Second, func() bool {
		st, _ := h.raw.GetSignature(ctx, sigKey)
		return st != nil && st.Mode == domain.ModeShadow && st.ConsecutiveConfirmations == 0
	})

	// The correction is itself in the audit trail with lineage (DR-005).
	log, _ := h.raw.AuditLog(ctx, 5)
	var found bool
	for _, r := range log {
		if r.CorrectsAuditID == audits[0].ID {
			found = true
		}
	}
	if !found {
		t.Error("correction lineage missing from audit trail")
	}
}

func TestConfigReloadPropagatesWithinBudget(t *testing.T) {
	// NFR-009 / SC-2: a config edit + nudge is reflected ≤ 1s.
	h := newHarness(t, "[thresholds]\napproval = 0.8\n")
	os.WriteFile(h.cfgPath, []byte("[thresholds]\napproval = 0.99\n"), 0o600)

	start := time.Now()
	if err := control.Nudge(context.Background(), h.ctlPath, control.KindReload); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool {
		cfg, _, _ := h.daemon.snapshot()
		return cfg.Thresholds.Approval == 0.99
	})
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("reload took %s, budget is 1s", elapsed)
	}
}

func TestLLMFallbackStagingRegateAndPromotion(t *testing.T) {
	// FR-010/SC-5: LLM staged decision is re-gated and promoted; timeout
	// and no-submit escalate.
	cfg := "[llm]\ncommand = [\"fake\"]\nauto_act = true\ntimeout_seconds = 5\n"
	h := newHarness(t, cfg)
	h.herdr.setPane(approvalPane)
	var requestID atomicString
	h.llm.configured = true
	h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		requestID.set(req.RequestID)
		id, err := h.raw.InsertLLMDecision(ctx, domain.LLMDecision{
			RequestID: req.RequestID, Signature: req.Signature,
			SituationType: req.SituationType, AgentType: req.AgentType,
			Action: "1", Rationale: "matches operator's usual approval",
			Status: "pending", CreatedAt: time.Now(),
		})
		if err != nil {
			return nil, err
		}
		return &domain.LLMDecision{
			ID: id, RequestID: req.RequestID, Action: "1",
			Rationale: "matches operator's usual approval", Status: "pending",
		}, nil
	}

	// Autonomous signature but history too mixed to clear the gate →
	// consult path.
	ctx := context.Background()
	s := classifierForTest().Classify("claude", "blocked", approvalPane)
	sig := domain.ComputeSignature(s)
	actions := []string{"1", "1", "1", "1", "2"}
	for i, a := range actions {
		h.raw.RecordDecision(ctx, domain.DecisionRecord{
			Signature: sig.Signature, SituationType: domain.SituationApproval,
			AgentType: "claude", ChosenAction: a, Source: domain.SourceOperator,
			CreatedAt: time.Now().Add(-time.Duration(len(actions)-i) * time.Minute),
		})
	}
	h.raw.UpsertSignature(ctx, domain.SignatureState{
		Signature: sig.Signature, SituationType: domain.SituationApproval,
		AgentType: "claude", Mode: domain.ModeAutonomous,
		ConsecutiveConfirmations: 8, UpdatedAt: time.Now(),
	})

	h.push("agent-13", "blocked")
	waitFor(t, 5*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	if got := h.herdr.sentInputs()[0]; got != "1" {
		t.Errorf("promoted LLM action %q, want \"1\"", got)
	}
	decs, _ := h.raw.LLMDecisionByRequest(ctx, requestID.get())
	if decs == nil || decs.Status != "accepted" {
		t.Errorf("staged decision should be accepted, got %+v", decs)
	}
}

type atomicString struct {
	mu sync.Mutex
	v  string
}

func (a *atomicString) set(v string) { a.mu.Lock(); a.v = v; a.mu.Unlock() }
func (a *atomicString) get() string  { a.mu.Lock(); defer a.mu.Unlock(); return a.v }

func TestLLMFailureEscalates(t *testing.T) {
	cfg := "[llm]\ncommand = [\"fake\"]\nauto_act = true\ntimeout_seconds = 1\n"
	h := newHarness(t, cfg)
	h.herdr.setPane(approvalPane)
	h.llm.configured = true
	h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		return nil, errors.New("llm timeout after 1s without submit_decision")
	}

	ctx := context.Background()
	s := classifierForTest().Classify("claude", "blocked", approvalPane)
	sig := domain.ComputeSignature(s)
	// Mostly-consistent history: above the variance guard's floor but below
	// the approval threshold, so the daemon takes the LLM consult path.
	for i, a := range []string{"1", "1", "1", "1", "2"} {
		h.raw.RecordDecision(ctx, domain.DecisionRecord{
			Signature: sig.Signature, SituationType: domain.SituationApproval,
			AgentType: "claude", ChosenAction: a, Source: domain.SourceOperator,
			CreatedAt: time.Now().Add(time.Duration(i) * time.Second),
		})
	}
	h.raw.UpsertSignature(ctx, domain.SignatureState{
		Signature: sig.Signature, SituationType: domain.SituationApproval,
		AgentType: "claude", Mode: domain.ModeAutonomous, UpdatedAt: time.Now(),
	})

	h.push("agent-14", "blocked")
	waitFor(t, 5*time.Second, func() bool {
		esc, _ := h.raw.PendingEscalations(ctx)
		return len(esc) == 1
	})
	if len(h.herdr.sentInputs()) != 0 {
		t.Fatal("LLM failure must escalate, not act")
	}
	esc, _ := h.raw.PendingEscalations(ctx)
	if !contains(esc[0].Rationale, "llm") && !contains(esc[0].Rationale, "LLM") {
		t.Errorf("escalation should cite the LLM failure: %+v", esc[0])
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
