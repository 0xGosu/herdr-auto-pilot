package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

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
	workspaces    []domain.WorkspaceInfo
	failSend      bool
	panicOnRead   bool
	failRead      bool
	// failReadOver, when > 0, fails ReadPane calls asking for more than
	// that many lines (isolates the deep LLM-context read from the
	// shallow classification read).
	failReadOver int
	readLines    []int
	paneInfo     domain.PaneInfo
	failPaneInfo bool
	// keys records every SendKey call (ports.KeystrokeSender). When frames
	// is set, "right"/"left" keys move frameIdx and ReadPane serves the
	// focused frame — simulating a multi-tab form under an arrow sweep.
	keys     []string
	frames   []string
	frameIdx int
	failKeys bool
	// failKeyName fails only SendKey calls for that specific key (e.g.
	// "left" to break the sweep's reset burst but not its Right sweep).
	failKeyName string
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
	f.readLines = append(f.readLines, lines)
	if f.failRead {
		return "", errors.New("induced read failure")
	}
	if f.failReadOver > 0 && lines > f.failReadOver {
		return "", errors.New("induced deep read failure")
	}
	if len(f.frames) > 0 {
		return f.frames[f.frameIdx], nil
	}
	return f.pane, nil
}

func (f *fakeHerdr) SendKey(ctx context.Context, paneID, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failKeys || (f.failKeyName != "" && key == f.failKeyName) {
		return errors.New("induced keystroke failure")
	}
	f.keys = append(f.keys, key)
	switch key {
	case "right":
		if f.frameIdx < len(f.frames)-1 {
			f.frameIdx++
		}
	case "left":
		if f.frameIdx > 0 {
			f.frameIdx--
		}
	}
	return nil
}

func (f *fakeHerdr) keysSent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.keys...)
}

func (f *fakeHerdr) setFrames(frames []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.frames = frames
	f.frameIdx = 0
}

func (f *fakeHerdr) PaneInfo(ctx context.Context, paneID string) (domain.PaneInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failPaneInfo {
		return domain.PaneInfo{}, errors.New("induced pane info failure")
	}
	return f.paneInfo, nil
}

func (f *fakeHerdr) readLineCalls() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.readLines...)
}

func (f *fakeHerdr) ListAgents(ctx context.Context) ([]domain.AgentTransition, error) {
	return nil, nil
}

func (f *fakeHerdr) ListWorkspaces(ctx context.Context) ([]domain.WorkspaceInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.WorkspaceInfo(nil), f.workspaces...), nil
}

func (f *fakeHerdr) ListTabs(ctx context.Context) ([]domain.TabInfo, error) {
	return nil, nil
}

func (f *fakeHerdr) setWorkspaces(wss []domain.WorkspaceInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.workspaces = wss
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

// fakeRewriter layers ports.RewriterPort on the LLM fake so the daemon's
// type assertion finds the optional rewrite capability.
type fakeRewriter struct {
	*fakeLLM
	mu       sync.Mutex
	rewrite  func(ctx context.Context, req domain.RewriteRequest) (string, error)
	requests []domain.RewriteRequest
}

func (f *fakeRewriter) RewriteConfigured() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rewrite != nil
}

func (f *fakeRewriter) Rewrite(ctx context.Context, req domain.RewriteRequest) (string, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	fn := f.rewrite
	f.mu.Unlock()
	if fn == nil {
		return "", errors.New("no rewrite configured")
	}
	return fn(ctx, req)
}

func (f *fakeRewriter) rewriteCalls() []domain.RewriteRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.RewriteRequest(nil), f.requests...)
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
	return newHarnessFull(t, cfgTOML, nil, nil)
}

// newHarnessWrapped lets a test substitute the HerdrPort the daemon sees
// (e.g. hiding optional interfaces) while keeping the fake for assertions.
func newHarnessWrapped(t *testing.T, cfgTOML string, wrap func(*fakeHerdr) ports.HerdrPort) *harness {
	return newHarnessFull(t, cfgTOML, wrap, nil)
}

// newHarnessRewriter installs a rewriter-capable LLM port.
func newHarnessRewriter(t *testing.T, cfgTOML string,
	rewrite func(ctx context.Context, req domain.RewriteRequest) (string, error)) (*harness, *fakeRewriter) {
	fr := &fakeRewriter{fakeLLM: &fakeLLM{}, rewrite: rewrite}
	return newHarnessFull(t, cfgTOML, nil, fr), fr
}

func newHarnessFull(t *testing.T, cfgTOML string, wrap func(*fakeHerdr) ports.HerdrPort, rw *fakeRewriter) *harness {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	// Existing tests assume near-synchronous attention handling: append a
	// tiny wildcard capture delay LAST, so a test-supplied
	// [[capture_delay]] rule earlier in the config still wins (first
	// match wins) while everything else skips the real-world settle.
	cfgTOML += tinyCaptureDelay
	if err := os.WriteFile(cfgPath, []byte(cfgTOML), 0o600); err != nil {
		t.Fatal(err)
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
	var llmPort ports.LLMPort = fl
	if rw != nil {
		fl = rw.fakeLLM
		llmPort = rw
	}
	var herdrPort ports.HerdrPort = fh
	if wrap != nil {
		herdrPort = wrap(fh)
	}

	// Socket paths must stay short for macOS (104-byte cap).
	ctlPath := filepath.Join(testutil.SocketDir(t), "control.sock")
	d, err := New(Options{
		ConfigPath:        cfgPath,
		ControlSocketPath: ctlPath,
		Store:             fs,
		Herdr:             herdrPort,
		Events:            fe,
		Notify:            fh,
		LLM:               llmPort,
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

// tinyCaptureDelay keeps tests near-synchronous; appended LAST to every
// harness config so a test-supplied [[capture_delay]] rule still wins
// (first match wins).
const tinyCaptureDelay = "\n[[capture_delay]]\nagent_type = \"*\"\nstart_ms = 1\nevent_ms = 1\n"

// writeConfig rewrites the harness config mid-test (callers nudge reload),
// re-appending the tiny capture delay so the daemon keeps test-speed
// capture timing after the reload.
func (h *harness) writeConfig(t *testing.T, cfgTOML string) {
	t.Helper()
	if err := os.WriteFile(h.cfgPath, []byte(cfgTOML+tinyCaptureDelay), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) push(agentID, status string) {
	h.pushIn(agentID, "", status)
}

func (h *harness) pushIn(agentID, workspaceID, status string) {
	h.events.ch <- domain.AgentTransition{
		AgentID: agentID, PaneID: agentID, WorkspaceID: workspaceID,
		AgentType: "claude", Status: status,
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

func TestLLMPromotionDeliversMenuDigitForLabel(t *testing.T) {
	// The LLM auto-act promotion path must also map an option LABEL to the
	// menu digit (Claude's numbered menu ignores the label).
	cfg := "[llm]\ncommand = [\"fake\"]\nauto_act = true\ntimeout_seconds = 5\n"
	h := newHarness(t, cfg)
	h.herdr.setPane(approvalPane)
	h.llm.configured = true
	h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		id, _ := h.raw.InsertLLMDecision(ctx, domain.LLMDecision{
			RequestID: req.RequestID, Signature: req.Signature,
			SituationType: req.SituationType, AgentType: req.AgentType,
			Action: "Yes", Rationale: "operator always approves", Status: "pending", CreatedAt: time.Now(),
		})
		return &domain.LLMDecision{ID: id, RequestID: req.RequestID, Action: "Yes",
			Rationale: "operator always approves", Status: "pending"}, nil
	}

	// A brand-new signature with an LLM configured takes the consult path.
	h.push("agent-llm-lbl", "blocked")
	waitFor(t, 5*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	if got := h.herdr.sentInputs()[0]; got != "1" {
		t.Errorf("promoted LLM label \"Yes\" delivered as %q, want digit \"1\"", got)
	}
}

func TestAutoActDeliversMenuDigitForLabelAction(t *testing.T) {
	// When the learned action is the option LABEL ("Yes"), the auto-act
	// path must deliver the menu digit "1" (Claude's numbered menu ignores
	// the label text) — the daemon-side half of the send-content fix.
	h := newHarness(t, "")
	h.herdr.setPane(approvalPane)
	h.seedAutonomous(approvalPane, domain.SituationApproval, "Yes")

	h.push("agent-lbl", "blocked")

	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	if got := h.herdr.sentInputs()[0]; got != "1" {
		t.Errorf("sent %q, want the menu digit \"1\" for label \"Yes\"", got)
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

func TestNeverAutoBlocksDestructiveApproval(t *testing.T) {
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
	if !contains(esc[0].Rationale, "pattern:") {
		t.Errorf("escalation should name the matched pattern, got %q", esc[0].Rationale)
	}
}

func TestNarrationInScrollbackDoesNotTripHeuristic(t *testing.T) {
	// Regression (agy false-positive): an agent whose *narration* discusses
	// destructive operations must not be perpetually flagged — the heuristic
	// scans the actionable region, not the whole scrollback.
	narration := "The cleanup job is deleting\nold databases tonight. That cannot be undone.\n"
	var filler strings.Builder
	for i := 0; i < domain.IrreversibleScanTailLines; i++ {
		filler.WriteString("filler narration text\n")
	}
	pane := narration + filler.String() + approvalPane
	h := newHarness(t, "")
	h.herdr.setPane(pane)
	h.seedAutonomous(pane, domain.SituationApproval, "1")

	h.push("agent-n1", "blocked")

	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	if got := h.herdr.sentInputs()[0]; got != "1" {
		t.Errorf("benign approval below stale narration should auto-act, sent %q", got)
	}
}

func TestNeverAutoScansFullSnapshotBeyondTailWindow(t *testing.T) {
	// FR-015 invariant pin: only the heuristic is scoped to the tail window;
	// the never-auto allowlist must keep scanning the entire snapshot.
	var b strings.Builder
	b.WriteString("Earlier: git push --force origin main\n")
	for i := 0; i < domain.IrreversibleScanTailLines; i++ {
		b.WriteString("filler narration text\n")
	}
	pane := b.String() + approvalPane
	h := newHarness(t, "")
	h.herdr.setPane(pane)
	h.seedAutonomous(pane, domain.SituationApproval, "1")

	h.push("agent-n6", "blocked")

	waitFor(t, 3*time.Second, func() bool {
		esc, _ := h.raw.PendingEscalations(context.Background())
		return len(esc) == 1
	})
	if len(h.herdr.sentInputs()) != 0 {
		t.Fatal("allowlist-matched content anywhere in the snapshot must block auto-action")
	}
	esc, _ := h.raw.PendingEscalations(context.Background())
	if !contains(esc[0].Rationale, "pattern:") {
		t.Errorf("escalation should name the matched pattern, got %q", esc[0].Rationale)
	}
}

func TestIdleNarrationEscalatesWithoutIrreversibleFlag(t *testing.T) {
	// Idle scans only the outbound next-task text; destructive words in the
	// recap must not surface as suspected_irreversible.
	pane := "I summarized the module: it guards against deleting\ndatabases and similar. Task complete.\n"
	h := newHarness(t, "")
	h.herdr.setPane(pane)

	h.push("agent-n2", "idle")

	waitFor(t, 3*time.Second, func() bool {
		esc, _ := h.raw.PendingEscalations(context.Background())
		return len(esc) == 1
	})
	esc, _ := h.raw.PendingEscalations(context.Background())
	if contains(esc[0].Rationale, "irreversible") {
		t.Errorf("idle recap must not be flagged irreversible, got %q", esc[0].Rationale)
	}
	if !contains(esc[0].Rationale, string(domain.ReasonNoTaskSource)) {
		t.Errorf("expected no_task_source escalation, got %q", esc[0].Rationale)
	}
}

func TestIrreversibleEscalationCitesIndicator(t *testing.T) {
	// FR-016 + diagnosability: a destructive-looking pending dialog escalates
	// and the rationale names the indicator and matched text.
	pane := "Do you want to proceed?\nThis action cannot be undone.\n❯ 1. Yes\n  2. No\n"
	h := newHarness(t, "")
	h.herdr.setPane(pane)
	h.seedAutonomous(pane, domain.SituationApproval, "1")

	h.push("agent-n3", "blocked")

	waitFor(t, 3*time.Second, func() bool {
		esc, _ := h.raw.PendingEscalations(context.Background())
		return len(esc) == 1
	})
	if len(h.herdr.sentInputs()) != 0 {
		t.Fatal("suspected-irreversible dialog must not be auto-approved")
	}
	esc, _ := h.raw.PendingEscalations(context.Background())
	if !contains(esc[0].Rationale, string(domain.ReasonSuspectedIrrevers)) {
		t.Fatalf("expected suspected_irreversible, got %q", esc[0].Rationale)
	}
	if !contains(esc[0].Rationale, "cannot be undone") || !contains(esc[0].Rationale, "indicator") {
		t.Errorf("rationale should cite the indicator and matched text, got %q", esc[0].Rationale)
	}
}

func TestAgentScopedIndicatorRules(t *testing.T) {
	// Operator indicator rules scoped to agent types: a rule listing the
	// agent applies; a rule scoped to other agents does not.
	pane := "Do you want to proceed?\nThis will frobnicate the widgets.\n❯ 1. Yes\n  2. No\n"
	cfgScoped := "[[safety.indicator_rules]]\npattern = '(?i)frobnicate\\s+the\\s+widgets'\nagents = [\"claude\"]\n"
	h := newHarness(t, cfgScoped)
	h.herdr.setPane(pane)
	h.seedAutonomous(pane, domain.SituationApproval, "1")
	h.push("agent-n4", "blocked")
	waitFor(t, 3*time.Second, func() bool {
		esc, _ := h.raw.PendingEscalations(context.Background())
		return len(esc) == 1
	})
	esc, _ := h.raw.PendingEscalations(context.Background())
	if !contains(esc[0].Rationale, "frobnicate") {
		t.Errorf("scoped rule should apply to a listed agent, got %q", esc[0].Rationale)
	}

	cfgOther := "[[safety.indicator_rules]]\npattern = '(?i)frobnicate\\s+the\\s+widgets'\nagents = [\"codex\", \"agy\"]\n"
	h2 := newHarness(t, cfgOther)
	h2.herdr.setPane(pane)
	h2.seedAutonomous(pane, domain.SituationApproval, "1")
	h2.push("agent-n5", "blocked")
	waitFor(t, 3*time.Second, func() bool { return len(h2.herdr.sentInputs()) == 1 })
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
	// escalate() persists the pause AFTER the escalation audit row, so the
	// pause may not be visible yet when PendingEscalations first reports 1 —
	// poll instead of asserting immediately (flaked under -race).
	waitFor(t, 3*time.Second, func() bool {
		rate, _ := h.raw.GetAgentRate(context.Background(), "agent-8")
		return rate.Paused
	})

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
	want := fmt.Sprintf("Your next task is step two. Read the full tasks list at %s.", taskFile)
	if got := h.herdr.sentInputs()[0]; got != want {
		t.Errorf("sent %q, want templated prompt for next unchecked item %q", got, want)
	}
}

func TestIdleDeclaredTaskCustomTemplate(t *testing.T) {
	// A per-source next_task_template overrides the outbound prompt format.
	dir := t.TempDir()
	taskFile := filepath.Join(dir, "tasks.md")
	os.WriteFile(taskFile, []byte("- [ ] polish docs\n"), 0o600)

	idlePane := "All tests pass. Task is complete.\n"
	cfg := fmt.Sprintf(
		"[[task_sources]]\nagent = \"agent-19\"\npath = %q\nnext_task_template = \"Task: {next_task_content} ({task_list_path})\"\n",
		taskFile)
	h := newHarness(t, cfg)
	h.herdr.setPane(idlePane)
	h.seedAutonomous(idlePane, domain.SituationIdle, domain.ActionNextDeclaredTask)

	h.push("agent-19", "idle")
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	want := fmt.Sprintf("Task: polish docs (%s)", taskFile)
	if got := h.herdr.sentInputs()[0]; got != want {
		t.Errorf("sent %q, want custom-templated prompt %q", got, want)
	}
}

func TestIdleDeclaredTaskRealTaskBeatsCompletedSource(t *testing.T) {
	// A later matching source with a real remaining task takes precedence
	// over an earlier matched source whose checklist is fully completed.
	dir := t.TempDir()
	doneFile := filepath.Join(dir, "done.md")
	os.WriteFile(doneFile, []byte("- [x] all finished\n"), 0o600)
	nextFile := filepath.Join(dir, "next.md")
	os.WriteFile(nextFile, []byte("- [ ] real task\n"), 0o600)

	idlePane := "All tests pass. Task is complete.\n"
	cfg := fmt.Sprintf(
		"[[task_sources]]\nagent = \"agent-21\"\npath = %q\n\n[[task_sources]]\nagent = \"agent-21\"\npath = %q\n",
		doneFile, nextFile)
	h := newHarness(t, cfg)
	h.herdr.setPane(idlePane)
	h.seedAutonomous(idlePane, domain.SituationIdle, domain.ActionNextDeclaredTask)

	h.push("agent-21", "idle")
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	want := fmt.Sprintf("Your next task is real task. Read the full tasks list at %s.", nextFile)
	if got := h.herdr.sentInputs()[0]; got != want {
		t.Errorf("sent %q, want the real remaining task to win over the completed source: %q", got, want)
	}
}

func TestIdleNonChecklistTaskFileDoesNotResolve(t *testing.T) {
	// A matched file without a single checklist item is not a completed
	// list: tier-1 must not send "none"; with no structured pane signal the
	// situation escalates instead.
	dir := t.TempDir()
	taskFile := filepath.Join(dir, "notes.md")
	os.WriteFile(taskFile, []byte("just prose notes, no checklist here\n"), 0o600)

	idlePane := "Task is complete. I could also look into performance sometime.\n"
	cfg := fmt.Sprintf("[[task_sources]]\nagent = \"agent-22\"\npath = %q\n", taskFile)
	h := newHarness(t, cfg)
	h.herdr.setPane(idlePane)
	h.seedAutonomous(idlePane, domain.SituationIdle, domain.ActionNextDeclaredTask)

	h.push("agent-22", "idle")
	waitFor(t, 3*time.Second, func() bool {
		esc, _ := h.raw.PendingEscalations(context.Background())
		return len(esc) == 1
	})
	if len(h.herdr.sentInputs()) != 0 {
		t.Fatal("a non-checklist task file must not produce a \"none\" prompt")
	}
}

func TestIdleDeclaredTaskCompletedListSendsNone(t *testing.T) {
	// A matched source with every item checked still delivers the templated
	// prompt with task content "none" instead of escalating.
	dir := t.TempDir()
	taskFile := filepath.Join(dir, "tasks.md")
	os.WriteFile(taskFile, []byte("- [x] step one\n- [x] step two\n"), 0o600)

	idlePane := "All tests pass. Task is complete.\n"
	cfg := fmt.Sprintf("[[task_sources]]\nagent = \"agent-20\"\npath = %q\n", taskFile)
	h := newHarness(t, cfg)
	h.herdr.setPane(idlePane)
	h.seedAutonomous(idlePane, domain.SituationIdle, domain.ActionNextDeclaredTask)

	h.push("agent-20", "idle")
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	want := fmt.Sprintf("Your next task is none. Read the full tasks list at %s.", taskFile)
	if got := h.herdr.sentInputs()[0]; got != want {
		t.Errorf("sent %q, want completed-list prompt %q", got, want)
	}
}

func TestIdleTaskSourceMatchesWorkspaceNameWildcard(t *testing.T) {
	// The workspace selector matches the workspace's herdr name (label)
	// with "*" wildcards, not the raw workspace id.
	dir := t.TempDir()
	taskFile := filepath.Join(dir, "tasks.md")
	os.WriteFile(taskFile, []byte("- [ ] workspace task\n"), 0o600)

	idlePane := "All tests pass. Task is complete.\n"
	cfg := fmt.Sprintf("[[task_sources]]\nworkspace = \"codex-*\"\npath = %q\n", taskFile)
	h := newHarness(t, cfg)
	h.herdr.setWorkspaces([]domain.WorkspaceInfo{{ID: "w7", Label: "codex-main"}})
	h.herdr.setPane(idlePane)
	h.seedAutonomous(idlePane, domain.SituationIdle, domain.ActionNextDeclaredTask)

	h.pushIn("agent-23", "w7", "idle")
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	want := fmt.Sprintf("Your next task is workspace task. Read the full tasks list at %s.", taskFile)
	if got := h.herdr.sentInputs()[0]; got != want {
		t.Errorf("sent %q, want workspace-name-matched prompt %q", got, want)
	}
}

func TestIdleTaskSourceWorkspaceNameMismatchEscalates(t *testing.T) {
	// A workspace selector that matches neither the workspace name nor the
	// id leaves the agent without a source; the idle situation escalates.
	dir := t.TempDir()
	taskFile := filepath.Join(dir, "tasks.md")
	os.WriteFile(taskFile, []byte("- [ ] other team's task\n"), 0o600)

	idlePane := "Task is complete. I could also look into performance sometime.\n"
	cfg := fmt.Sprintf("[[task_sources]]\nworkspace = \"*-vscode3\"\npath = %q\n", taskFile)
	h := newHarness(t, cfg)
	h.herdr.setWorkspaces([]domain.WorkspaceInfo{{ID: "w7", Label: "codex-main"}})
	h.herdr.setPane(idlePane)
	h.seedAutonomous(idlePane, domain.SituationIdle, domain.ActionNextDeclaredTask)

	h.pushIn("agent-24", "w7", "idle")
	waitFor(t, 3*time.Second, func() bool {
		esc, _ := h.raw.PendingEscalations(context.Background())
		return len(esc) == 1
	})
	if len(h.herdr.sentInputs()) != 0 {
		t.Fatal("a non-matching workspace selector must not deliver the source's task")
	}
}

func TestIdleTaskSourceWorkspaceIdFallback(t *testing.T) {
	// When no workspace name resolves (empty listing), the selector still
	// matches the raw workspace id so existing id-based configs keep working.
	dir := t.TempDir()
	taskFile := filepath.Join(dir, "tasks.md")
	os.WriteFile(taskFile, []byte("- [ ] fallback task\n"), 0o600)

	idlePane := "All tests pass. Task is complete.\n"
	cfg := fmt.Sprintf("[[task_sources]]\nworkspace = \"w9\"\npath = %q\n", taskFile)
	h := newHarness(t, cfg)
	h.herdr.setPane(idlePane)
	h.seedAutonomous(idlePane, domain.SituationIdle, domain.ActionNextDeclaredTask)

	h.pushIn("agent-25", "w9", "idle")
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	want := fmt.Sprintf("Your next task is fallback task. Read the full tasks list at %s.", taskFile)
	if got := h.herdr.sentInputs()[0]; got != want {
		t.Errorf("sent %q, want id-fallback-matched prompt %q", got, want)
	}
}

func TestIdleTaskSourceMatchesAgentShortName(t *testing.T) {
	// Task-source selectors match the auto-generated (or renamed) short
	// name, not just pane ids.
	dir := t.TempDir()
	taskFile := filepath.Join(dir, "tasks.md")
	os.WriteFile(taskFile, []byte("- [ ] short-name task\n"), 0o600)

	idlePane := "All tests pass. Task is complete.\n"
	h := newHarness(t, "")
	h.herdr.setPane(idlePane)
	h.seedAutonomous(idlePane, domain.SituationIdle, domain.ActionNextDeclaredTask)

	// Name the agent the way the daemon will, then rename it and point a
	// task source at the new name.
	ctx := context.Background()
	if _, err := h.raw.EnsureAgentName(ctx, "agent-15"); err != nil {
		t.Fatal(err)
	}
	if err := h.raw.RenameAgent(ctx, "agent-15", "docs-writer"); err != nil {
		t.Fatal(err)
	}
	cfgTOML := fmt.Sprintf("[[task_sources]]\nagent = \"docs-writer\"\npath = %q\n", taskFile)
	h.writeConfig(t, cfgTOML)
	if err := control.Nudge(ctx, h.ctlPath, control.KindReload); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, func() bool {
		cfg, _, _ := h.daemon.snapshot()
		return len(cfg.TaskSources) == 1
	})

	h.push("agent-15", "idle")
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	want := fmt.Sprintf("Your next task is short-name task. Read the full tasks list at %s.", taskFile)
	if got := h.herdr.sentInputs()[0]; got != want {
		t.Errorf("sent %q, want the short-name-matched task prompt %q", got, want)
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

	// Wait for the lineage audit — the LAST write applyCorrection makes
	// before resolving — so every earlier effect (demotion included) is
	// durably visible when we assert; waiting on the demotion alone raced
	// the lineage append on slow runners.
	waitFor(t, 3*time.Second, func() bool {
		log, _ := h.raw.AuditLog(ctx, 5)
		for _, r := range log {
			if r.CorrectsAuditID == audits[0].ID {
				return true
			}
		}
		return false
	})

	st, err := h.raw.GetSignature(ctx, sigKey)
	if err != nil || st == nil {
		t.Fatalf("signature state: %v %v", st, err)
	}
	if st.Mode != domain.ModeShadow || st.ConsecutiveConfirmations != 0 {
		t.Errorf("correction must demote to shadow with a reset streak: %+v", st)
	}
}

func TestConfigReloadPropagatesWithinBudget(t *testing.T) {
	// NFR-009 / SC-2: a config edit + nudge is reflected ≤ 1s.
	h := newHarness(t, "[thresholds]\napproval = 0.8\n")
	h.writeConfig(t, "[thresholds]\napproval = 0.99\n")

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

func TestLLMConfidentScoreShownOnEscalation(t *testing.T) {
	// The agent's self-reported confident_score (0-100) must reach the
	// escalation entry the operator sees; without one (-1) nothing is added.
	cfg := "[llm]\ncommand = [\"fake\"]\ntimeout_seconds = 5\n" // auto_act off → shadow reject
	h := newHarness(t, cfg)
	h.herdr.setPane(approvalPane)
	h.llm.configured = true
	h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		id, err := h.raw.InsertLLMDecision(ctx, domain.LLMDecision{
			RequestID: req.RequestID, Signature: req.Signature,
			SituationType: req.SituationType, AgentType: req.AgentType,
			Action: "1", Rationale: "operator always approves",
			ConfidentScore: 62, Status: "pending", CreatedAt: time.Now(),
		})
		if err != nil {
			return nil, err
		}
		return &domain.LLMDecision{
			ID: id, RequestID: req.RequestID, Action: "1",
			Rationale: "operator always approves", ConfidentScore: 62, Status: "pending",
		}, nil
	}

	ctx := context.Background()
	h.push("agent-cs", "blocked")
	var esc []domain.AuditRecord
	waitFor(t, 5*time.Second, func() bool {
		esc, _ = h.raw.PendingEscalations(ctx)
		return len(esc) == 1
	})
	if !strings.Contains(esc[0].Rationale, "llm confidence 62/100") {
		t.Errorf("escalation rationale should carry the confident score, got %q", esc[0].Rationale)
	}
	if !strings.Contains(esc[0].Rationale, "[shadow_mode]") {
		t.Errorf("shadow-mode reject expected, got %q", esc[0].Rationale)
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

func TestAgentNamedOnDiscoveryAndWorking(t *testing.T) {
	// A brand-new agent must get its short name the moment it is seen —
	// on a "detected" discovery event or a "working" transition — not only
	// when it first needs attention.
	h := newHarness(t, "")
	ctx := context.Background()

	h.push("w23:p5", "detected")
	waitFor(t, 2*time.Second, func() bool {
		names, _ := h.raw.AgentNames(ctx)
		return names["w23:p5"] != ""
	})

	h.push("w24:p1", "working")
	waitFor(t, 2*time.Second, func() bool {
		names, _ := h.raw.AgentNames(ctx)
		return names["w24:p1"] != ""
	})

	// Names are two-word adjective-animal slugs, and a detected event never
	// triggers any automated action.
	names, _ := h.raw.AgentNames(ctx)
	for _, id := range []string{"w23:p5", "w24:p1"} {
		if !strings.Contains(names[id], "-") {
			t.Errorf("agent %s name %q should be a two-word slug", id, names[id])
		}
	}
	if inputs := h.herdr.sentInputs(); len(inputs) != 0 {
		t.Errorf("discovery must not cause sends, got %v", inputs)
	}
}

// captureConsultContext wires the fake LLM to record req.ContextJSON and
// fail the consult (the escalation path is not under test here).
func captureConsultContext(h *harness) *atomicString {
	var captured atomicString
	h.llm.configured = true
	h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		captured.set(req.ContextJSON)
		return nil, errors.New("consult not under test")
	}
	return &captured
}

func decodeContext(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("context JSON unparseable: %v (%s)", err, raw)
	}
	return m
}

func TestConsultContextCarriesLocationCwdAndDeepExcerpt(t *testing.T) {
	// The consult context must hand the LLM the agent's herdr location
	// (workspace/tab/pane ids), the pane cwd, and a pane excerpt sourced
	// from a deeper read than the classification snapshot.
	h := newHarness(t, "")
	captured := captureConsultContext(h)
	filler := strings.Repeat("compiling module alpha beta gamma\n", 200) // ~6800 chars
	h.herdr.setPane(filler + approvalPane)
	h.herdr.mu.Lock()
	h.herdr.paneInfo = domain.PaneInfo{
		PaneID: "w2:p7", TabID: "w2:t3", WorkspaceID: "w2",
		Cwd: "/home/op/project", ForegroundCwd: "/home/op/project/sub",
	}
	h.herdr.mu.Unlock()

	// Fresh signature + configured LLM → consult (no seeding needed).
	h.events.ch <- domain.AgentTransition{
		AgentID: "w2:p7", PaneID: "w2:p7", TabID: "w2:t3", WorkspaceID: "w2",
		AgentType: "claude", Status: "blocked",
	}
	waitFor(t, 5*time.Second, func() bool { return captured.get() != "" })

	m := decodeContext(t, captured.get())
	for key, want := range map[string]string{
		"workspace_id":   "w2",
		"tab_id":         "w2:t3",
		"pane_id":        "w2:p7",
		"agent_id":       "w2:p7",
		"cwd":            "/home/op/project",
		"foreground_cwd": "/home/op/project/sub",
	} {
		if got, _ := m[key].(string); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	excerpt, _ := m["pane_excerpt"].(string)
	if len(excerpt) != 5000 {
		t.Errorf("pane_excerpt length = %d, want the 5000-char default", len(excerpt))
	}
	if !strings.Contains(excerpt, "Do you want to proceed?") {
		t.Error("pane_excerpt must keep the tail of the pane")
	}
	// The deep read asks for pane_excerpt_chars/10 lines; the shallow
	// classification read keeps the PaneReadLines default.
	lines := h.herdr.readLineCalls()
	if len(lines) < 2 || lines[len(lines)-1] != 500 {
		t.Errorf("deep read should request 500 lines, got calls %v", lines)
	}
}

func TestConsultContextExcerptSizeConfigurable(t *testing.T) {
	h := newHarness(t, "[llm]\ncommand = [\"fake\"]\npane_excerpt_chars = 300\n")
	captured := captureConsultContext(h)
	h.herdr.setPane(strings.Repeat("noise line for the excerpt budget\n", 40) + approvalPane)

	h.push("agent-ex", "blocked")
	waitFor(t, 5*time.Second, func() bool { return captured.get() != "" })

	m := decodeContext(t, captured.get())
	excerpt, _ := m["pane_excerpt"].(string)
	if len(excerpt) != 300 {
		t.Errorf("pane_excerpt length = %d, want the configured 300", len(excerpt))
	}
}

func TestConsultContextDegradesWithoutPaneInfo(t *testing.T) {
	// A failing `pane get` must not block the consult: location falls back
	// to the transition ids and cwd stays empty.
	h := newHarness(t, "")
	captured := captureConsultContext(h)
	h.herdr.setPane(approvalPane)
	h.herdr.mu.Lock()
	h.herdr.failPaneInfo = true
	h.herdr.mu.Unlock()

	h.events.ch <- domain.AgentTransition{
		AgentID: "w3:p1", PaneID: "w3:p1", TabID: "w3:t1", WorkspaceID: "w3",
		AgentType: "claude", Status: "blocked",
	}
	waitFor(t, 5*time.Second, func() bool { return captured.get() != "" })

	m := decodeContext(t, captured.get())
	if got, _ := m["cwd"].(string); got != "" {
		t.Errorf("cwd should degrade to empty, got %q", got)
	}
	if got, _ := m["tab_id"].(string); got != "w3:t1" {
		t.Errorf("tab_id should come from the transition, got %q", got)
	}
}

func TestConsultContextFallsBackToClassificationSnapshot(t *testing.T) {
	// When the deep read fails, the excerpt falls back to the (shallow)
	// classification snapshot instead of aborting the consult.
	h := newHarness(t, "")
	captured := captureConsultContext(h)
	h.herdr.setPane(approvalPane)
	h.herdr.mu.Lock()
	h.herdr.failReadOver = 120 // classification read (120 lines) passes, deep read fails
	h.herdr.mu.Unlock()

	h.push("agent-fb", "blocked")
	waitFor(t, 5*time.Second, func() bool { return captured.get() != "" })

	m := decodeContext(t, captured.get())
	if excerpt, _ := m["pane_excerpt"].(string); excerpt != approvalPane {
		t.Errorf("excerpt should fall back to the classification snapshot, got %q", excerpt)
	}
}

// inspectorlessHerdr exposes only the base HerdrPort surface, hiding the
// fake's PaneInfo to exercise the optional-InspectorPort degradation.
type inspectorlessHerdr struct{ f *fakeHerdr }

func (h inspectorlessHerdr) Send(ctx context.Context, paneID, input string) error {
	return h.f.Send(ctx, paneID, input)
}

func (h inspectorlessHerdr) ReadPane(ctx context.Context, paneID string, lines int) (string, error) {
	return h.f.ReadPane(ctx, paneID, lines)
}

func (h inspectorlessHerdr) ListAgents(ctx context.Context) ([]domain.AgentTransition, error) {
	return h.f.ListAgents(ctx)
}

func TestConsultContextWithoutInspectorPort(t *testing.T) {
	// An adapter without the optional InspectorPort still consults: ids
	// come from the transition and cwd stays empty.
	h := newHarnessWrapped(t, "", func(f *fakeHerdr) ports.HerdrPort {
		return inspectorlessHerdr{f}
	})
	captured := captureConsultContext(h)
	h.herdr.setPane(approvalPane)

	h.events.ch <- domain.AgentTransition{
		AgentID: "w4:p1", PaneID: "w4:p1", TabID: "w4:t2", WorkspaceID: "w4",
		AgentType: "claude", Status: "blocked",
	}
	waitFor(t, 5*time.Second, func() bool { return captured.get() != "" })

	m := decodeContext(t, captured.get())
	if got, _ := m["cwd"].(string); got != "" {
		t.Errorf("cwd must stay empty without InspectorPort, got %q", got)
	}
	if got, _ := m["tab_id"].(string); got != "w4:t2" {
		t.Errorf("tab_id should come from the transition, got %q", got)
	}
	if got, _ := m["workspace_id"].(string); got != "w4" {
		t.Errorf("workspace_id should come from the transition, got %q", got)
	}
}

func TestTailTrimsAtRuneBoundary(t *testing.T) {
	if got := tail("hello", 10); got != "hello" {
		t.Errorf("short input must pass through, got %q", got)
	}
	if got := tail("abcdef", 3); got != "def" {
		t.Errorf("ascii cut = %q, want \"def\"", got)
	}
	// A 4-byte budget on "héllo" lands inside the 2-byte é: the leading
	// continuation byte must be skipped, never emitted.
	if got := tail("héllo", 4); got != "llo" {
		t.Errorf("mid-rune cut = %q, want \"llo\"", got)
	}
	if got := tail("héllo", 5); got != "éllo" {
		t.Errorf("boundary cut = %q, want \"éllo\"", got)
	}
	if got := tail(strings.Repeat("界", 10), 7); !utf8.ValidString(got) {
		t.Errorf("tail must never emit invalid UTF-8, got %q", got)
	}
}

// Every classified situation records the pane snapshot its signature was
// first seen with (rule provenance); later differing renders never
// overwrite the original.
func TestSignatureSnapshotRecordedOnFirstSighting(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setPane(approvalPane)
	sig := h.seedAutonomous(approvalPane, domain.SituationApproval, "1")

	h.push("agent-snap", "blocked")
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })

	ctx := context.Background()
	snap, err := h.raw.GetSignatureSnapshot(ctx, sig)
	if err != nil || !contains(snap, "Do you want to proceed?") {
		t.Fatalf("snapshot should hold the classification pane, got %q err=%v", snap, err)
	}

	// A second transition with slightly different content (same signature —
	// options unchanged) must keep the original snapshot.
	h.herdr.setPane("EXTRA NARRATION\n" + approvalPane)
	h.push("agent-snap", "blocked")
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 2 })
	snap2, _ := h.raw.GetSignatureSnapshot(ctx, sig)
	if snap2 != snap {
		t.Errorf("later sighting must not overwrite the original snapshot")
	}
}
