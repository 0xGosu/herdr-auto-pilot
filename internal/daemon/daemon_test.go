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
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
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
	// keyScript updates pane content after selected keystrokes. It models
	// interactive forms whose visible state changes after a digit/Enter rather
	// than only after Left/Right navigation.
	keyScript       []string
	keyScriptFrames []string
	// agents is the current agent set ListAgents reports; listAgentsCalls
	// counts calls so a test can assert the retry guard short-circuits before
	// resolving the pane.
	agents          []domain.AgentTransition
	listAgentsCalls int
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
	if len(f.keyScript) > 0 && len(f.keyScriptFrames) > 0 && key == f.keyScript[0] {
		f.keyScript = f.keyScript[1:]
		f.pane = f.keyScriptFrames[0]
		f.keyScriptFrames = f.keyScriptFrames[1:]
		f.frames = nil
		f.frameIdx = 0
	}
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

func (f *fakeHerdr) SendKeys(ctx context.Context, paneID string, keys ...string) error {
	for _, key := range keys {
		if err := f.SendKey(ctx, paneID, key); err != nil {
			return err
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

func (f *fakeHerdr) setKeyScript(initial string, keys, frames []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pane = initial
	f.frames = nil
	f.frameIdx = 0
	f.keyScript = append([]string(nil), keys...)
	f.keyScriptFrames = append([]string(nil), frames...)
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
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listAgentsCalls++
	return append([]domain.AgentTransition(nil), f.agents...), nil
}

func (f *fakeHerdr) setAgents(agents []domain.AgentTransition) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.agents = agents
}

func (f *fakeHerdr) listAgentsCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listAgentsCalls
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

// fakeTaskGen layers ports.TaskGeneratorPort on the LLM fake so the daemon's
// type assertion finds the optional idle task-generation capability.
type fakeTaskGen struct {
	*fakeLLM
	mu       sync.Mutex
	generate func(ctx context.Context, req domain.TaskGenRequest) (string, error)
	requests []domain.TaskGenRequest
}

func (f *fakeTaskGen) GenerateTaskConfigured() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.generate != nil
}

func (f *fakeTaskGen) GenerateTask(ctx context.Context, req domain.TaskGenRequest) (string, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	fn := f.generate
	f.mu.Unlock()
	if fn == nil {
		return "", errors.New("no generate configured")
	}
	return fn(ctx, req)
}

func (f *fakeTaskGen) genCalls() []domain.TaskGenRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.TaskGenRequest(nil), f.requests...)
}

// failingStore injects persistence failures on audit writes (FR-024) and,
// optionally, on the LLM in-flight check.
type failingStore struct {
	ports.StorePort
	mu          sync.Mutex
	failAudit   bool
	failPending bool
}

func (f *failingStore) setFailAudit(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failAudit = v
}

func (f *failingStore) setFailPending(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failPending = v
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

func (f *failingStore) HasPendingLLMConsult(ctx context.Context, agentID string) (bool, error) {
	f.mu.Lock()
	fail := f.failPending
	f.mu.Unlock()
	if fail {
		return false, errors.New("induced pending-check failure")
	}
	return f.StorePort.HasPendingLLMConsult(ctx, agentID)
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

// newHarnessTaskGen installs a task-generator-capable LLM port for idle
// task-suggestion tests.
func newHarnessTaskGen(t *testing.T, cfgTOML string,
	generate func(ctx context.Context, req domain.TaskGenRequest) (string, error)) (*harness, *fakeTaskGen) {
	tg := &fakeTaskGen{fakeLLM: &fakeLLM{}, generate: generate}
	return newHarnessCore(t, cfgTOML, nil, tg, tg.fakeLLM), tg
}

func newHarnessFull(t *testing.T, cfgTOML string, wrap func(*fakeHerdr) ports.HerdrPort, rw *fakeRewriter) *harness {
	fl := &fakeLLM{}
	var llmPort ports.LLMPort = fl
	if rw != nil {
		fl = rw.fakeLLM
		llmPort = rw
	}
	return newHarnessCore(t, cfgTOML, wrap, llmPort, fl)
}

// newHarnessCore wires the daemon with a caller-supplied LLM port (plus the
// underlying *fakeLLM for assertions), so optional-capability variants
// (rewriter, task generator) share one setup path.
func newHarnessCore(t *testing.T, cfgTOML string, wrap func(*fakeHerdr) ports.HerdrPort,
	llmPort ports.LLMPort, fl *fakeLLM) *harness {
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

const codexPlanApprovalPane = `plan tail

Implement this plan?

› 1. Yes, implement this plan          Switch to Default and start coding.
  2. Yes, clear context and implement  Fresh thread. Context: 20% used.
  3. No, stay in Plan mode             Continue planning with the model.

Press enter to confirm or esc to go back
`

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
	if !contains(audits[0].PaneExcerpt, "Do you want to proceed") {
		t.Errorf("auto audit must carry the classified pane content, got %q", audits[0].PaneExcerpt)
	}
}

func TestManualCaptureNudgeUsesNormalPipeline(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setPane(approvalPane)
	h.herdr.setAgents([]domain.AgentTransition{{
		AgentID: "agent-manual", PaneID: "agent-manual", AgentType: "claude", Status: "blocked",
	}})

	if err := control.NudgeCapture(context.Background(), h.ctlPath, "agent-manual"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool {
		audits, _ := h.raw.AuditLog(context.Background(), 10)
		for _, audit := range audits {
			if audit.AgentID == "agent-manual" && audit.Trigger == "manual-capture: blocked" {
				return audit.SituationType == domain.SituationApproval
			}
		}
		return false
	})
}

func TestManualCaptureRecognizesIdleCodexPlanApproval(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setPane(codexPlanApprovalPane)
	h.herdr.setAgents([]domain.AgentTransition{{
		AgentID: "agent-codex-plan", PaneID: "agent-codex-plan", AgentType: "codex", Status: "idle",
	}})

	if err := control.NudgeCapture(context.Background(), h.ctlPath, "agent-codex-plan"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool {
		audits, _ := h.raw.AuditLog(context.Background(), 10)
		for _, audit := range audits {
			if audit.AgentID != "agent-codex-plan" || audit.Trigger != "manual-capture: idle" {
				continue
			}
			if audit.SituationType != domain.SituationApproval {
				t.Fatalf("Codex Plan capture situation = %s, want approval", audit.SituationType)
			}
			if !strings.HasPrefix(audit.Signature, "approval:") ||
				!strings.Contains(audit.PaneExcerpt, "Implement this plan?") {
				t.Fatalf("Codex Plan capture was not preserved precisely: %+v", audit)
			}
			return true
		}
		return false
	})
}

func TestLLMPromotionDeliversMenuDigitForLabel(t *testing.T) {
	// The LLM auto-act promotion path must also map an option LABEL to the
	// menu digit (Claude's numbered menu ignores the label).
	cfg := "[llm]\ncommand = [\"fake\"]\nauto_act_confidence_threshold = 50\ntimeout_seconds = 5\n"
	h := newHarness(t, cfg)
	h.herdr.setPane(approvalPane)
	h.llm.configured = true
	h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		id, _ := h.raw.InsertLLMDecision(ctx, domain.LLMDecision{
			RequestID: req.RequestID, Signature: req.Signature,
			SituationType: req.SituationType, AgentType: req.AgentType,
			Action: "Yes", Rationale: "operator always approves", ConfidentScore: 80, Status: "pending", CreatedAt: time.Now(),
		})
		return &domain.LLMDecision{ID: id, RequestID: req.RequestID, Action: "Yes",
			Rationale: "operator always approves", ConfidentScore: 80, Status: "pending"}, nil
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

// TestConfirmDrivenShadowToAutoPromotion is the end-to-end regression for the
// live-observed learning loop: an operator's repeated confirmations of a
// shadow-mode approval grow the rule's agreement and promote it from shadow to
// autonomous once GraduationN consistent confirmations AND a confidence above
// threshold both hold — after which the daemon auto-answers the approval with
// the numbered-menu DIGIT (not the learned label text). Guards three things at
// once: confirm-driven promotion, confidence growth, and menu-digit selection.
func TestConfirmDrivenShadowToAutoPromotion(t *testing.T) {
	const graduationN = 3
	h := newHarness(t, fmt.Sprintf("[learning]\ngraduation_n = %d\n", graduationN))
	h.herdr.setPane(approvalPane)
	ctx := context.Background()

	// The learned action is the option LABEL "Yes" (approvalPane offers
	// "1. Yes / 2. No, ..."); proving the auto-act delivers digit "1" is the
	// menu-digit-selection half of the regression.
	const learned = "Yes"

	// Seed a MIXED shadow history so agreement starts below 1.0 and can be
	// observed to climb as consistent confirmations accumulate (an all-
	// consistent history pins the recency-weighted agreement at 1.0 — no growth
	// to assert). "No" is the oldest decision; two "Yes" keep it dominant.
	s := classifierForTest().Classify("claude", "blocked", approvalPane)
	if s.Type != domain.SituationApproval {
		t.Fatalf("fixture classifies as %v, want approval", s.Type)
	}
	sig := domain.ComputeSignature(s)
	for i, action := range []string{"No", learned, learned} { // oldest → newest
		if _, err := h.raw.RecordDecision(ctx, domain.DecisionRecord{
			Signature: sig.Signature, SituationType: domain.SituationApproval,
			AgentType: "claude", ChosenAction: action, Source: domain.SourceOperator,
			CreatedAt: time.Now().Add(-time.Duration(3-i) * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}

	app := frontend.App{Store: h.raw, Herdr: h.herdr, ControlPath: h.ctlPath, Author: "test"}

	// Phase 1: each shadow escalation is confirmed record-only (like
	// `hap confirm` — a learning event, no pane send), growing the rule until
	// it graduates on the GraduationN-th confirmation.
	var confidences []float64
	for i := 1; i <= graduationN; i++ {
		h.push("agent-promote", "blocked")

		var esc domain.AuditRecord
		waitFor(t, 3*time.Second, func() bool {
			pend, _ := h.raw.PendingEscalations(ctx)
			if len(pend) != 1 {
				return false
			}
			esc = pend[0]
			return true
		})
		// Shadow mode must never act: no input has reached the pane yet.
		if n := len(h.herdr.sentInputs()); n != 0 {
			t.Fatalf("shadow mode sent %d inputs before graduation; want 0", n)
		}
		if esc.Suggestion != "respond: "+learned {
			t.Fatalf("shadow escalation suggestion = %q, want %q", esc.Suggestion, "respond: "+learned)
		}

		// Operator confirms the suggested action (learn-only, send=false).
		if err := app.Confirm(ctx, esc.ID, false); err != nil {
			t.Fatal(err)
		}

		// Wait for the confirmation to be learned AND the escalation resolved —
		// both, so the next push re-escalates instead of tripping the
		// duplicate-pending guard.
		var st *domain.SignatureState
		waitFor(t, 3*time.Second, func() bool {
			st, _ = h.raw.GetSignature(ctx, sig.Signature)
			pend, _ := h.raw.PendingEscalations(ctx)
			return st != nil && st.ConsecutiveConfirmations == i && len(pend) == 0
		})

		wantMode := domain.ModeShadow
		if i >= graduationN {
			wantMode = domain.ModeAutonomous
		}
		if st.Mode != wantMode {
			t.Fatalf("after %d confirmation(s): mode = %q, want %q", i, st.Mode, wantMode)
		}
		confidences = append(confidences, st.CachedConfidence)
	}

	// Confidence growth: the recency-weighted agreement climbs with each
	// consistent confirmation (do not assert exact floats — assert the curve).
	for i := 1; i < len(confidences); i++ {
		if !(confidences[i] > confidences[i-1]) {
			t.Errorf("confidence must grow with each confirmation, got %v", confidences)
			break
		}
	}
	// A friendlier restatement of the loop invariant above (strict growth at
	// every step implies last > first): it surfaces a clearer message if the
	// per-step check ever regresses to a partial climb.
	if !(confidences[len(confidences)-1] > confidences[0]) {
		t.Errorf("final confidence %.3f must exceed initial %.3f",
			confidences[len(confidences)-1], confidences[0])
	}

	// Each confirmation was recorded as an operator-confirmed learning event
	// (DR-005 lineage) — the surface `hap audit` reads back.
	audits, err := h.raw.AuditLog(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	confirmed := 0
	for _, a := range audits {
		if a.Trigger == domain.TriggerOperatorCorrection && a.Rationale == domain.RationaleOperatorConfirmed {
			confirmed++
		}
	}
	if confirmed != graduationN {
		t.Errorf("operator-confirmed lineage rows = %d, want %d", confirmed, graduationN)
	}

	// Phase 2: now autonomous, the next matching approval is AUTO-answered with
	// the menu DIGIT for the learned LABEL — no operator in the loop.
	h.push("agent-promote", "blocked")
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	if got := h.herdr.sentInputs()[0]; got != "1" {
		t.Errorf("auto-act delivered %q, want the menu digit \"1\" for label %q", got, learned)
	}

	// The auto-act audit row carries the learned-rule rationale (the surface
	// operators read as "... chosen N times (agreement ... > threshold ...)").
	var auto *domain.AuditRecord
	post, err := h.raw.AuditLog(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	for i := range post {
		if post[i].Status == "auto" {
			auto = &post[i]
			break
		}
	}
	if auto == nil {
		t.Fatal("no auto-act audit row after graduation")
	}
	if !contains(auto.Rationale, "chosen") || !contains(auto.Rationale, "agreement") {
		t.Errorf("auto-act rationale missing learned-rule surface: %q", auto.Rationale)
	}
	if auto.Input != learned {
		t.Errorf("auto-act audit input = %q, want %q", auto.Input, learned)
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

// otherApprovalPane is a distinct approval (different command → different
// classified content/signature) used to prove content-level dedup.
const otherApprovalPane = "Bash(npm install)\n\nDo you want to proceed?\n❯ 1. Yes\n  2. No, and tell the agent what to do differently\n"

// ignoredRows returns every audit row the daemon marked as an ignored
// duplicate event.
func ignoredRows(t *testing.T, h *harness) []domain.AuditRecord {
	t.Helper()
	audits, err := h.raw.AuditLog(context.Background(), 50)
	if err != nil {
		t.Fatalf("audit log: %v", err)
	}
	var out []domain.AuditRecord
	for _, a := range audits {
		if a.Status == "ignored" {
			out = append(out, a)
		}
	}
	return out
}

// TestPipelineIgnoresDuplicateEvent covers the live-event dedup: a fresh
// transition whose captured situation exactly matches an escalation still
// awaiting the user is dropped as a no-op and audited, while a genuinely new
// situation on the same agent still escalates (content-level, not agent-level).
func TestPipelineIgnoresDuplicateEvent(t *testing.T) {
	h := newHarness(t, "")
	ctx := context.Background()
	h.herdr.setPane(approvalPane)

	// First blocked event with no learned rule escalates.
	h.push("agent-dup", "blocked")
	waitFor(t, 3*time.Second, func() bool {
		esc, _ := h.raw.PendingEscalations(ctx)
		return len(esc) == 1
	})

	// An identical event (same agent + type + pane content) while the first
	// escalation is still pending is a duplicate: no new escalation, no send,
	// and a single audit row explaining the ignore.
	h.push("agent-dup", "blocked")
	waitFor(t, 3*time.Second, func() bool { return len(ignoredRows(t, h)) == 1 })

	if esc, _ := h.raw.PendingEscalations(ctx); len(esc) != 1 {
		t.Fatalf("duplicate event created a second escalation: got %d, want 1", len(esc))
	}
	if len(h.herdr.sentInputs()) != 0 {
		t.Fatal("duplicate event must not send input")
	}
	ign := ignoredRows(t, h)[0]
	if ign.Rationale != "duplicated event" {
		t.Errorf("ignored rationale = %q, want %q", ign.Rationale, "duplicated event")
	}
	if !contains(ign.PaneExcerpt, "Do you want to proceed") {
		t.Errorf("ignored row should keep the captured pane content, got %q", ign.PaneExcerpt)
	}

	// A genuinely NEW situation on the SAME agent (different pane content) is
	// NOT a duplicate — content-level dedup, not agent-level — so it escalates.
	h.herdr.setPane(otherApprovalPane)
	h.push("agent-dup", "blocked")
	waitFor(t, 3*time.Second, func() bool {
		esc, _ := h.raw.PendingEscalations(ctx)
		return len(esc) == 2
	})
	if got := len(ignoredRows(t, h)); got != 1 {
		t.Fatalf("new situation was wrongly ignored: %d ignored rows, want 1", got)
	}
}

// TestReconcileEscalatesAlreadyParkedAgent covers #49: an agent already
// blocked at subscribe time (never delivered as a pane.agent_status_changed
// transition) is surfaced by reconcileAttention through the normal
// capture→classify→escalate path. The second reconcile proves the dedup guard
// against escalation storms on repeated sweeps.
func TestReconcileEscalatesAlreadyParkedAgent(t *testing.T) {
	h := newHarness(t, "")
	ctx := context.Background()
	h.herdr.setPane(approvalPane)
	// Parked BEFORE any transition arrives — only ListAgents knows it exists.
	h.herdr.setAgents([]domain.AgentTransition{
		{AgentID: "pA", PaneID: "pA", AgentType: "claude", Status: "blocked"},
	})

	h.daemon.reconcileAttention(ctx)

	waitFor(t, 3*time.Second, func() bool {
		esc, _ := h.raw.PendingEscalations(ctx)
		return len(esc) == 1
	})
	if len(h.herdr.sentInputs()) != 0 {
		t.Fatal("no learned rule: reconcile must escalate, not send input")
	}

	// Second reconcile (a resubscribe/sweep re-run) must not re-escalate.
	// reconcileAttention is synchronous and, with the episode already handled
	// (and an open escalation on record), returns without scheduling a capture.
	h.daemon.reconcileAttention(ctx)
	if esc, _ := h.raw.PendingEscalations(ctx); len(esc) != 1 {
		t.Fatalf("resubscribe storm re-escalated: got %d escalations, want 1", len(esc))
	}
}

// TestReconcileAutoAnswersParkedAgentOnce proves the in-memory dedup guard for
// the auto-answer path: a confident learned rule leaves NO escalation row, so
// only episodeHandled (not the store check) stops a re-answer on the next
// sweep. A genuine "working" transition re-arms the pane for a new episode.
func TestReconcileAutoAnswersParkedAgentOnce(t *testing.T) {
	h := newHarness(t, "")
	ctx := context.Background()
	h.herdr.setPane(approvalPane)
	h.seedAutonomous(approvalPane, domain.SituationApproval, "1")
	h.herdr.setAgents([]domain.AgentTransition{
		{AgentID: "pA", PaneID: "pA", AgentType: "claude", Status: "blocked"},
	})

	h.daemon.reconcileAttention(ctx)
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	if got := h.herdr.sentInputs()[0]; got != "1" {
		t.Errorf("sent %q, want learned approval \"1\"", got)
	}
	if esc, _ := h.raw.PendingEscalations(ctx); len(esc) != 0 {
		t.Fatalf("auto-answered agent should not escalate, got %d", len(esc))
	}

	// Second reconcile: episode handled, no escalation row to lean on — the
	// in-memory guard alone must prevent a duplicate send. Synchronous return.
	h.daemon.reconcileAttention(ctx)
	if n := len(h.herdr.sentInputs()); n != 1 {
		t.Fatalf("sweep re-answered a still-parked agent: got %d sends, want 1", n)
	}

	// A real "working" transition ends the episode: the in-memory guard clears
	// so a future block/idle/done is surfaced again (re-arm).
	h.push("pA", "working")
	waitFor(t, 2*time.Second, func() bool {
		h.daemon.mu.Lock()
		defer h.daemon.mu.Unlock()
		return !h.daemon.episodeHandled["pA"]
	})
}

// TestReconcileIgnoresNonAttentionAgents confirms only blocked/idle/done panes
// are reconciled — a working agent is left alone.
func TestReconcileIgnoresNonAttentionAgents(t *testing.T) {
	h := newHarness(t, "")
	ctx := context.Background()
	h.herdr.setPane(approvalPane)
	h.herdr.setAgents([]domain.AgentTransition{
		{AgentID: "pA", PaneID: "pA", AgentType: "claude", Status: "working"},
	})

	h.daemon.reconcileAttention(ctx)

	if esc, _ := h.raw.PendingEscalations(ctx); len(esc) != 0 {
		t.Fatalf("working agent must not reconcile, got %d escalations", len(esc))
	}
	if n := len(h.herdr.sentInputs()); n != 0 {
		t.Fatalf("working agent must not be actioned, got %d sends", n)
	}
}

// TestReconcileDurableGuardSurvivesRestart exercises the cross-restart half of
// the dedup: after a restart the in-memory episode guard is empty, so the
// escalation row on disk is the only thing that stops a duplicate. It also
// verifies the AgentID round-trip — the pipeline must stamp the escalation with
// the agent id reconcile matches on, or hasOpenEscalation silently no-ops.
func TestReconcileDurableGuardSurvivesRestart(t *testing.T) {
	h := newHarness(t, "")
	ctx := context.Background()
	h.herdr.setPane(approvalPane)
	h.herdr.setAgents([]domain.AgentTransition{
		{AgentID: "pA", PaneID: "pA", AgentType: "claude", Status: "blocked"},
	})

	h.daemon.reconcileAttention(ctx) // real pipeline escalation
	waitFor(t, 3*time.Second, func() bool {
		esc, _ := h.raw.PendingEscalations(ctx)
		return len(esc) == 1
	})

	// Simulate a restart: drop the in-memory guard, keeping the escalation row.
	// The durable guard alone must now prevent a re-drive.
	h.daemon.mu.Lock()
	delete(h.daemon.episodeHandled, "pA")
	h.daemon.mu.Unlock()

	h.daemon.reconcileAttention(ctx)
	// reconcileAttention is synchronous up to scheduleCapture: a broken guard
	// leaves a pending capture entry; a working one schedules nothing.
	h.daemon.mu.Lock()
	_, scheduled := h.daemon.pendingCapture["pA"]
	h.daemon.mu.Unlock()
	if scheduled {
		t.Fatal("durable guard failed: reconcile re-drove an agent with an open escalation")
	}
	if esc, _ := h.raw.PendingEscalations(ctx); len(esc) != 1 {
		t.Fatalf("open escalation duplicated across restart: got %d, want 1", len(esc))
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
	if !contains(esc[0].Rationale, "pattern") || !contains(esc[0].Rationale, `matched "git push --force"`) {
		t.Errorf("escalation should name the matched pattern and excerpt, got %q", esc[0].Rationale)
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

func TestNeverAutoIgnoresDestructiveScrollbackBeyondTailWindow(t *testing.T) {
	// FR-015 scoping: the never-auto allowlist scans only the actionable region
	// (pending dialog), not the whole scrollback. A destructive command left in
	// stale scrollback above the tail window must not veto a benign pending
	// approval — that was the false-alarm source.
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

	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	if got := h.herdr.sentInputs()[0]; got != "1" {
		t.Errorf("benign approval below stale destructive scrollback should auto-act, sent %q", got)
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
	// and the rationale names the heuristic pattern and matched text.
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
	if !contains(esc[0].Rationale, "cannot be undone") || !contains(esc[0].Rationale, "pattern") {
		t.Errorf("rationale should cite the heuristic pattern and matched text, got %q", esc[0].Rationale)
	}
}

func TestAgentScopedNeverAutoRules(t *testing.T) {
	// Operator never-auto rules scoped to agent types: a rule listing the
	// agent applies; a rule scoped to other agents does not.
	pane := "Do you want to proceed?\nThis will frobnicate the widgets.\n❯ 1. Yes\n  2. No\n"
	cfgScoped := "[[safety.never_auto_rules]]\npattern = '(?i)frobnicate\\s+the\\s+widgets'\nagent_types = [\"claude\"]\n"
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

	cfgOther := "[[safety.never_auto_rules]]\npattern = '(?i)frobnicate\\s+the\\s+widgets'\nagent_types = [\"codex\", \"agy\"]\n"
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

func TestPaneReadFailureDedupsDuplicateEvents(t *testing.T) {
	// A persistent Herdr/pane-read outage delivers the same blocked event
	// repeatedly. That escalation path bypasses escalate() (there is no pane
	// to classify), so it must dedup inline — an outage must not pile up one
	// identical pending escalation per event.
	h := newHarness(t, "")
	h.herdr.failRead = true
	ctx := context.Background()

	h.push("agent-ru", "blocked")
	waitFor(t, 3*time.Second, func() bool {
		esc, _ := h.raw.PendingEscalations(ctx)
		return len(esc) == 1
	})

	// Second identical event during the outage is a duplicate: ignored, no
	// second pending escalation.
	h.push("agent-ru", "blocked")
	waitFor(t, 3*time.Second, func() bool { return len(ignoredRows(t, h)) == 1 })

	esc, _ := h.raw.PendingEscalations(ctx)
	if len(esc) != 1 {
		t.Fatalf("read-outage storm created duplicate escalations: got %d, want 1", len(esc))
	}
	if !contains(esc[0].Rationale, string(domain.ReasonHerdrUnreachable)) {
		t.Errorf("expected herdr_unreachable escalation, got %q", esc[0].Rationale)
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
	// agent pauses until human interaction. Pin explicit limits so the test
	// exercises the CONSECUTIVE guard specifically, independent of the
	// defaults (and with the per-minute cap held high so it never interferes).
	h := newHarness(t, "[limits]\nmax_consecutive_auto_prompts = 5\nmax_auto_prompts_per_minute = 1000\n")
	h.herdr.setPane(approvalPane)
	h.seedAutonomous(approvalPane, domain.SituationApproval, "1")

	for i := 0; i < 5; i++ {
		h.push("agent-8", "blocked")
		want := i + 1
		waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == want })
	}
	// 6th: blocked + escalated, retaining the blocked action so the operator
	// can explicitly confirm and send it.
	h.push("agent-8", "blocked")
	var escalation domain.AuditRecord
	waitFor(t, 3*time.Second, func() bool {
		esc, _ := h.raw.PendingEscalations(context.Background())
		if len(esc) != 1 {
			return false
		}
		escalation = esc[0]
		return true
	})
	if len(h.herdr.sentInputs()) != 5 {
		t.Fatalf("6th consecutive prompt must be blocked; sent %d", len(h.herdr.sentInputs()))
	}
	if escalation.Suggestion != "respond: 1" {
		t.Fatalf("rate-limit escalation must carry the blocked action, got %q", escalation.Suggestion)
	}
	// escalate() persists the pause AFTER the escalation audit row, so the
	// pause may not be visible yet when PendingEscalations first reports 1 —
	// poll instead of asserting immediately (flaked under -race).
	waitFor(t, 3*time.Second, func() bool {
		rate, _ := h.raw.GetAgentRate(context.Background(), "agent-8")
		return rate.Paused
	})

	// Confirming the escalation is a human-initiated send: it delivers the
	// retained action and resumes this agent's automation.
	app := frontend.App{Store: h.raw, Herdr: h.herdr, ControlPath: h.ctlPath, Author: "test"}
	if err := app.Confirm(context.Background(), escalation.ID, true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 6 })
	if got := h.herdr.sentInputs()[5]; got != "1" {
		t.Fatalf("confirmed rate-limited action = %q, want %q", got, "1")
	}
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

	name, err := h.raw.EnsureAgentName(context.Background(), "agent-9")
	if err != nil {
		t.Fatal(err)
	}
	h.push("agent-9", "idle")
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	want := (&domain.DeclaredTask{Task: "step two", Path: taskFile, AgentName: name}).Prompt()
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

func TestIdleDeclaredTaskAgentNameTemplate(t *testing.T) {
	// {agent_name} in a next_task_template renders the agent's resolved
	// short name in the delivered prompt.
	dir := t.TempDir()
	taskFile := filepath.Join(dir, "tasks.md")
	os.WriteFile(taskFile, []byte("- [ ] polish docs\n"), 0o600)

	idlePane := "All tests pass. Task is complete.\n"
	cfg := fmt.Sprintf(
		"[[task_sources]]\nagent = \"agent-name-19\"\npath = %q\nnext_task_template = \"Hey {agent_name}, next: {next_task_content}\"\n",
		taskFile)
	h := newHarness(t, cfg)
	h.herdr.setPane(idlePane)
	h.seedAutonomous(idlePane, domain.SituationIdle, domain.ActionNextDeclaredTask)

	name, err := h.raw.EnsureAgentName(context.Background(), "agent-name-19")
	if err != nil {
		t.Fatal(err)
	}
	h.push("agent-name-19", "idle")
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	want := fmt.Sprintf("Hey %s, next: polish docs", name)
	if got := h.herdr.sentInputs()[0]; got != want {
		t.Errorf("sent %q, want agent-name-templated prompt %q", got, want)
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

	name, err := h.raw.EnsureAgentName(context.Background(), "agent-21")
	if err != nil {
		t.Fatal(err)
	}
	h.push("agent-21", "idle")
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	want := (&domain.DeclaredTask{Task: "real task", Path: nextFile, AgentName: name}).Prompt()
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

	name, err := h.raw.EnsureAgentName(context.Background(), "agent-20")
	if err != nil {
		t.Fatal(err)
	}
	h.push("agent-20", "idle")
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	want := (&domain.DeclaredTask{Task: domain.NoTaskContent, Path: taskFile, AgentName: name}).Prompt()
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

	name, err := h.raw.EnsureAgentName(context.Background(), "agent-23")
	if err != nil {
		t.Fatal(err)
	}
	h.pushIn("agent-23", "w7", "idle")
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	want := (&domain.DeclaredTask{Task: "workspace task", Path: taskFile, AgentName: name}).Prompt()
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

	name, err := h.raw.EnsureAgentName(context.Background(), "agent-25")
	if err != nil {
		t.Fatal(err)
	}
	h.pushIn("agent-25", "w9", "idle")
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	want := (&domain.DeclaredTask{Task: "fallback task", Path: taskFile, AgentName: name}).Prompt()
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
	want := (&domain.DeclaredTask{Task: "short-name task", Path: taskFile, AgentName: "docs-writer"}).Prompt()
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

func TestIdleGeneratesTaskSuggestionEscalation(t *testing.T) {
	// FR-011 relaxation: idle with no task source and task_generate_command
	// configured surfaces an LLM-suggested task as a (non-retryable)
	// escalation, and sends nothing to the pane.
	idlePane := "Task is complete.\n"
	h, tg := newHarnessTaskGen(t, "", func(ctx context.Context, req domain.TaskGenRequest) (string, error) {
		return "Investigate the flaky auth test and add a retry guard", nil
	})
	h.herdr.setPane(idlePane)

	ctx := context.Background()
	h.push("agent-20", "idle")
	var esc []domain.AuditRecord
	waitFor(t, 3*time.Second, func() bool {
		esc, _ = h.raw.PendingEscalations(ctx)
		return len(esc) == 1
	})
	want := domain.SuggestTaskPrefix + "Investigate the flaky auth test and add a retry guard"
	if esc[0].Suggestion != want {
		t.Errorf("escalation suggestion = %q, want %q", esc[0].Suggestion, want)
	}
	if domain.IsRetryableLLMEscalation(&esc[0]) {
		t.Error("a successful task suggestion must NOT be retryable (operator confirms/dismisses)")
	}
	if calls := tg.genCalls(); len(calls) != 1 {
		t.Fatalf("generator should be called once, got %d", len(calls))
	} else if calls[0].AgentType != "claude" && calls[0].AgentType != "unknown" {
		t.Errorf("generator request should carry the agent type, got %q", calls[0].AgentType)
	}
	if len(h.herdr.sentInputs()) != 0 {
		t.Fatalf("nothing may be sent to the pane before the operator confirms, sent %v", h.herdr.sentInputs())
	}
	// The pending guard was cleared on the outcome path.
	if pending, _ := h.raw.HasPendingLLMConsult(ctx, "agent-20"); pending {
		t.Error("a resolved task generation must not leave a pending request")
	}
}

func TestIdleTaskGenFailureIsRetryableEscalation(t *testing.T) {
	// A failed generation surfaces the rationale and is retryable with `l`.
	h, _ := newHarnessTaskGen(t, "", func(ctx context.Context, req domain.TaskGenRequest) (string, error) {
		return "", errors.New("generate-task CLI failed: boom")
	})
	h.herdr.setPane("Task is complete.\n")

	ctx := context.Background()
	h.push("agent-21", "idle")
	var esc []domain.AuditRecord
	waitFor(t, 3*time.Second, func() bool {
		esc, _ = h.raw.PendingEscalations(ctx)
		return len(esc) == 1
	})
	if !domain.IsRetryableLLMEscalation(&esc[0]) {
		t.Fatalf("a failed generation should be retryable, got rationale %q", esc[0].Rationale)
	}
	if !strings.Contains(esc[0].Rationale, string(domain.ReasonTaskGenFailed)) ||
		!strings.Contains(esc[0].Rationale, "boom") {
		t.Errorf("rationale should carry the failure tag and message, got %q", esc[0].Rationale)
	}
}

func TestIdleTaskGenRetryReDrivesGeneration(t *testing.T) {
	// A queued retry re-injects the idle status (not blocked) so the pane
	// re-classifies as idle and re-enters the generate-task path.
	var mu sync.Mutex
	calls := 0
	h, _ := newHarnessTaskGen(t, "", func(ctx context.Context, req domain.TaskGenRequest) (string, error) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			return "", errors.New("generate-task timeout after 5s")
		}
		return "Write the missing unit tests for the parser", nil
	})
	h.herdr.setPane("Task is complete.\n")

	ctx := context.Background()
	h.push("agent-22", "idle")
	var esc []domain.AuditRecord
	waitFor(t, 3*time.Second, func() bool {
		esc, _ = h.raw.PendingEscalations(ctx)
		return len(esc) == 1 && domain.IsRetryableLLMEscalation(&esc[0])
	})

	// Agent still live on its pane: queue a retry and nudge.
	h.herdr.setAgents([]domain.AgentTransition{{AgentID: "agent-22", PaneID: "agent-22", Status: "idle"}})
	if _, err := h.raw.InsertLLMRetry(ctx, esc[0].ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := control.Nudge(ctx, h.ctlPath, control.KindReload); err != nil {
		t.Fatal(err)
	}

	// The retry re-drove generation, which this time yielded a suggestion.
	want := domain.SuggestTaskPrefix + "Write the missing unit tests for the parser"
	waitFor(t, 5*time.Second, func() bool {
		all, _ := h.raw.PendingEscalations(ctx)
		for _, e := range all {
			if e.Suggestion == want {
				return true
			}
		}
		return false
	})
}

func TestIdleTaskGenDropsStaleSuggestion(t *testing.T) {
	// If the agent starts work while the LLM runs (its live herdr status is no
	// longer idle when the result returns), the suggestion is DROPPED — never
	// surfaced, so it can't be confirmed and sent into a busy agent.
	var h *harness
	h, tg := newHarnessTaskGen(t, "", func(ctx context.Context, req domain.TaskGenRequest) (string, error) {
		// Simulate the agent moving on mid-generation.
		h.herdr.setAgents([]domain.AgentTransition{{AgentID: "agent-30", PaneID: "agent-30", Status: "working"}})
		return "Some now-stale task", nil
	})
	h.herdr.setPane("Task is complete.\n")

	ctx := context.Background()
	h.push("agent-30", "idle")
	// The drop path clears the pending row and writes NO audit, so once the
	// outcome is processed (pending cleared, generator ran) the assertion is
	// stable.
	waitFor(t, 3*time.Second, func() bool {
		pending, _ := h.raw.HasPendingLLMConsult(ctx, "agent-30")
		return !pending && len(tg.genCalls()) == 1
	})
	esc, _ := h.raw.PendingEscalations(ctx)
	if len(esc) != 0 {
		t.Errorf("a stale suggestion must not be surfaced, got %d escalations", len(esc))
	}
	if len(h.herdr.sentInputs()) != 0 {
		t.Errorf("nothing may be sent for a dropped suggestion, got %v", h.herdr.sentInputs())
	}
}

func TestErrorRetryCeilingEndToEnd(t *testing.T) {
	// FR-014: up to 2 automated retries per error signature; 3rd escalates.
	// A Claude error/retry situation (interrupt prompt) — generic build-log
	// text no longer classifies as error (it is ordinary narration).
	errorPane := "⎿  Interrupted · What should Claude do instead?\n"
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
	h := newHarness(t, "[confidence_thresholds]\napproval = 0.8\n")
	h.writeConfig(t, "[confidence_thresholds]\napproval = 0.99\n")

	start := time.Now()
	if err := control.Nudge(context.Background(), h.ctlPath, control.KindReload); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool {
		cfg, _, _ := h.daemon.snapshot()
		return cfg.ConfidenceThresholds.Approval == 0.99
	})
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("reload took %s, budget is 1s", elapsed)
	}
}

func TestLLMFallbackStagingRegateAndPromotion(t *testing.T) {
	// FR-010/SC-5: LLM staged decision is re-gated and promoted; timeout
	// and no-submit escalate.
	// Pin approval = 0.8 so the seeded 0.73 history stays below threshold and
	// takes the consult path (the default dropped to 0.70 in c8b3e82).
	cfg := "[llm]\ncommand = [\"fake\"]\nauto_act_confidence_threshold = 50\ntimeout_seconds = 5\n[confidence_thresholds]\napproval = 0.8\n"
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
			ConfidentScore: 80, Status: "pending", CreatedAt: time.Now(),
		})
		if err != nil {
			return nil, err
		}
		return &domain.LLMDecision{
			ID: id, RequestID: req.RequestID, Action: "1",
			Rationale: "matches operator's usual approval", ConfidentScore: 80, Status: "pending",
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
	// The auto-acted audit row captures BOTH scores: the LLM's self-reported
	// 0-100 and the computed 0-1 agreement over this signature's history
	// (4×"1" vs 1×"2" → 0.8-ish, always > 0), so the Audit view never shows an
	// LLM=80 row alongside a misleading conf=0.00.
	audits, _ := h.raw.AuditLog(ctx, 10)
	var promoted *domain.AuditRecord
	for i := range audits {
		if audits[i].Action == "auto:1" && audits[i].Trigger == "llm-fallback" {
			promoted = &audits[i]
			break
		}
	}
	if promoted == nil {
		t.Fatalf("no promoted llm-fallback audit row found in %+v", audits)
	}
	if promoted.LLMConfidence == nil || *promoted.LLMConfidence != 80 {
		t.Errorf("promoted row LLMConfidence = %v, want 80", promoted.LLMConfidence)
	}
	if promoted.Confidence <= 0 {
		t.Errorf("promoted row computed Confidence = %v, want > 0", promoted.Confidence)
	}
}

func TestLLMConfidentScoreShownOnEscalation(t *testing.T) {
	// The agent's self-reported confident_score (0-100) must reach the
	// escalation entry the operator sees; without one (-1) nothing is added.
	cfg := "[llm]\ncommand = [\"fake\"]\ntimeout_seconds = 5\n" // threshold defaults to 999: 62 < 999 → escalate
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
	if !strings.Contains(esc[0].Rationale, "[llm_low_confidence]") {
		t.Errorf("below-threshold reject expected, got %q", esc[0].Rationale)
	}
	// The score is also captured as a structured field on the audit row, not
	// only embedded in the rationale prose.
	if esc[0].LLMConfidence == nil || *esc[0].LLMConfidence != 62 {
		t.Errorf("escalation LLMConfidence = %v, want 62", esc[0].LLMConfidence)
	}
}

func TestAutoActConfidenceThresholdGate(t *testing.T) {
	// The LLM decision auto-acts only when its confidence meets the operator's
	// threshold; below it (or with no reported score) the situation escalates
	// with [llm_low_confidence].
	cases := []struct {
		name      string
		threshold int
		score     int
		promote   bool
	}{
		{"above threshold promotes", 50, 80, true},
		{"at threshold promotes (inclusive)", 70, 70, true},
		{"below threshold escalates", 70, 40, false},
		{"reported 0 promotes at threshold 0", 0, 0, true},
		{"unreported (-1) escalates even at threshold 0", 0, -1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := fmt.Sprintf("[llm]\ncommand = [\"fake\"]\nauto_act_confidence_threshold = %d\ntimeout_seconds = 5\n", tc.threshold)
			h := newHarness(t, cfg)
			h.herdr.setPane(approvalPane)
			h.llm.configured = true
			h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
				dec := domain.LLMDecision{
					RequestID: req.RequestID, Signature: req.Signature,
					SituationType: req.SituationType, AgentType: req.AgentType,
					Action: "1", Rationale: "matches operator", ConfidentScore: tc.score,
					Status: "pending", CreatedAt: time.Now(),
				}
				id, _ := h.raw.InsertLLMDecision(ctx, dec)
				dec.ID = id
				return &dec, nil
			}

			ctx := context.Background()
			h.push("agent-thr", "blocked")
			if tc.promote {
				waitFor(t, 5*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
				if got := h.herdr.sentInputs()[0]; got != "1" {
					t.Errorf("promoted action = %q, want \"1\"", got)
				}
				return
			}
			waitFor(t, 5*time.Second, func() bool {
				esc, _ := h.raw.PendingEscalations(ctx)
				return len(esc) == 1
			})
			if len(h.herdr.sentInputs()) != 0 {
				t.Fatalf("below-threshold decision must not act, sent %v", h.herdr.sentInputs())
			}
			esc, _ := h.raw.PendingEscalations(ctx)
			if !strings.Contains(esc[0].Rationale, "[llm_low_confidence]") {
				t.Errorf("expected llm_low_confidence escalation, got %q", esc[0].Rationale)
			}
			// The structured LLM score on the escalation row follows the
			// not-reported convention: an unreported score (-1) must land as
			// nil, NOT a stored -1; a reported-but-low score is kept verbatim.
			got := esc[0].LLMConfidence
			if tc.score < 0 {
				if got != nil {
					t.Errorf("unreported score must store nil LLMConfidence, got %v", *got)
				}
			} else if got == nil || *got != tc.score {
				t.Errorf("escalation LLMConfidence = %v, want %d", got, tc.score)
			}
		})
	}
}

type atomicString struct {
	mu sync.Mutex
	v  string
}

func (a *atomicString) set(v string) { a.mu.Lock(); a.v = v; a.mu.Unlock() }
func (a *atomicString) get() string  { a.mu.Lock(); defer a.mu.Unlock(); return a.v }

func TestLLMFailureEscalates(t *testing.T) {
	// Pin approval = 0.8 so the seeded 0.73 history stays below threshold and
	// takes the consult path (the default dropped to 0.70 in c8b3e82).
	cfg := "[llm]\ncommand = [\"fake\"]\nauto_act_confidence_threshold = 0\ntimeout_seconds = 1\n[confidence_thresholds]\napproval = 0.8\n"
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

func TestRetryLLMReDrivesConsultAsFreshEscalation(t *testing.T) {
	// A failed consult leaves a retryable escalation; a queued retry re-drives
	// a fresh consult against the agent's live pane. Even a high-confidence
	// result returns to the operator as a new escalation instead of auto-acting.
	cfg := "[llm]\ncommand = [\"fake\"]\nauto_act_confidence_threshold = 50\ntimeout_seconds = 5\n"
	h := newHarness(t, cfg)
	h.herdr.setPane(approvalPane)
	h.llm.configured = true

	var mu sync.Mutex
	calls := 0
	h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			// First consult times out → the escalation the operator retries.
			return nil, errors.New("llm timeout after 5s without submit_decision")
		}
		// The retry consult submits with high confidence, but retry intent forces
		// the result back to human review.
		id, _ := h.raw.InsertLLMDecision(ctx, domain.LLMDecision{
			RequestID: req.RequestID, Signature: req.Signature,
			SituationType: req.SituationType, AgentType: req.AgentType,
			Action: "@noop", Rationale: "no reply is needed", ConfidentScore: 99, Status: "pending", CreatedAt: time.Now(),
		})
		return &domain.LLMDecision{ID: id, RequestID: req.RequestID, Action: "@noop",
			Rationale: "no reply is needed", ConfidentScore: 99, Status: "pending"}, nil
	}

	ctx := context.Background()
	// Brand-new signature + configured LLM → consult path.
	h.push("agent-retry", "blocked")
	var esc []domain.AuditRecord
	waitFor(t, 5*time.Second, func() bool {
		esc, _ = h.raw.PendingEscalations(ctx)
		return len(esc) == 1
	})
	if !domain.IsRetryableLLMEscalation(&esc[0]) {
		t.Fatalf("first consult should leave a retryable LLM escalation, got %q", esc[0].Rationale)
	}
	// The failed consult cleared its pending request (the guard is reset on
	// every outcome path, including the failure branch), so a retry is allowed.
	if pending, _ := h.raw.HasPendingLLMConsult(ctx, "agent-retry"); pending {
		t.Fatal("a resolved (failed) consult must not leave a pending request")
	}

	// The agent is still live on its pane: queue a retry and nudge.
	h.herdr.setAgents([]domain.AgentTransition{{AgentID: "agent-retry", PaneID: "agent-retry", Status: "blocked"}})
	if _, err := h.raw.InsertLLMRetry(ctx, esc[0].ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := control.Nudge(ctx, h.ctlPath, control.KindReload); err != nil {
		t.Fatal(err)
	}

	// The retry re-drove the consult and created a distinct review item.
	var fresh domain.AuditRecord
	waitFor(t, 5*time.Second, func() bool {
		pending, _ := h.raw.PendingEscalations(ctx)
		for _, row := range pending {
			if row.AgentID == "agent-retry" && row.ID != esc[0].ID {
				fresh = row
				return true
			}
		}
		return false
	})
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Fatalf("retry result must not auto-act, sent inputs: %q", got)
	}
	if fresh.Suggestion != "LLM suggested: "+domain.ActionNoopSuggestion {
		t.Errorf("fresh escalation suggestion = %q, want a no-reply suggestion", fresh.Suggestion)
	}
	if !strings.Contains(fresh.Rationale, "["+string(domain.ReasonLLMRetry)+"]") ||
		!strings.Contains(fresh.Rationale, fmt.Sprintf("#%d", esc[0].ID)) ||
		!strings.Contains(fresh.Rationale, "no reply is needed") {
		t.Errorf("fresh escalation should identify its retry source, got %q", fresh.Rationale)
	}
	if original, _ := h.raw.GetAudit(ctx, esc[0].ID); original == nil || original.Status != "retried" {
		t.Errorf("accepted retry must retire its source escalation, got %+v", original)
	}
}

func TestRetryLLMOutcomeNeverAutoActs(t *testing.T) {
	// Exercise the promotion boundary without starting Run (and therefore
	// without a control socket): retry provenance must override a high model
	// confidence and turn @noop into a fresh, reviewable escalation.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[llm]\nauto_act_confidence_threshold = 50\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	fh := &fakeHerdr{}
	d, err := New(Options{ConfigPath: cfgPath, Store: raw, Herdr: fh, LLM: &fakeLLM{}})
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	s := domain.Situation{
		Type: domain.SituationApproval, AgentID: "agent-retry-boundary",
		AgentType: "claude", PaneID: "pane-retry-boundary", Status: "blocked",
		Content: approvalPane, RetryAuditID: 297,
	}
	sig := domain.ComputeSignature(s)
	request := domain.LLMRequest{
		RequestID: "retry-boundary-request", AgentID: s.AgentID,
		Signature: sig.Signature, SituationType: s.Type, AgentType: s.AgentType,
		RetryAuditID: 297,
	}
	decisionID, err := raw.InsertLLMDecision(ctx, domain.LLMDecision{
		RequestID: request.RequestID, Signature: sig.Signature,
		SituationType: s.Type, AgentType: s.AgentType,
		Action: "@noop", Rationale: "no reply is needed", ConfidentScore: 99,
		Status: "pending", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	d.handleLLMOutcome(ctx, llmOutcome{
		situation: s, sig: sig, request: request,
		decision: &domain.LLMDecision{
			ID: decisionID, RequestID: request.RequestID, Action: "@noop",
			Rationale: "no reply is needed", ConfidentScore: 99, Status: "pending",
		},
	})

	if got := fh.sentInputs(); len(got) != 0 {
		t.Fatalf("retry result auto-acted, sent inputs: %q", got)
	}
	pending, err := raw.PendingEscalations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("retry result created %d pending escalations, want 1: %+v", len(pending), pending)
	}
	if pending[0].Suggestion != "LLM suggested: "+domain.ActionNoopSuggestion {
		t.Errorf("retry suggestion = %q, want no-reply suggestion", pending[0].Suggestion)
	}
	if !strings.Contains(pending[0].Rationale, "[llm_retry]") ||
		!strings.Contains(pending[0].Rationale, "#297") ||
		!strings.Contains(pending[0].Rationale, "no reply is needed") {
		t.Errorf("retry rationale lost provenance or fresh reasoning: %q", pending[0].Rationale)
	}
}

func TestRetryLLMGuardSkipsWhileConsultInFlight(t *testing.T) {
	// The retry must never stack onto a consult that is still running: with a
	// pending llm_requests row for the agent, the retry short-circuits BEFORE
	// resolving the pane, and is still drained from the queue.
	h := newHarness(t, "")
	ctx := context.Background()
	id, _ := h.raw.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "agent-busy", Signature: "sig", Trigger: "agent-status: blocked",
		SituationType: domain.SituationApproval, Action: "escalated",
		Rationale: "[llm_timeout] llm timeout", Status: "escalated", CreatedAt: time.Now(),
	})
	if _, err := h.raw.StageLLMRequest(ctx, domain.LLMRequest{
		RequestID: "req-agent-busy-1", Signature: "sig", SituationType: domain.SituationApproval,
		AgentType: "claude", AgentID: "agent-busy", ContextJSON: "{}", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	h.herdr.setAgents([]domain.AgentTransition{{AgentID: "agent-busy", PaneID: "agent-busy", Status: "blocked"}})

	if _, err := h.raw.InsertLLMRetry(ctx, id, time.Now()); err != nil {
		t.Fatal(err)
	}
	// Drain directly for determinism. ListAgents is the guard's witness: a
	// retry resolves an agent's pane through it, so a skipped retry adds no
	// call. The startup attention reconcile (#49) also calls ListAgents once,
	// so measure the DELTA around processLLMRetries rather than an absolute
	// count — after waiting for that one reconcile call to land.
	waitFor(t, 2*time.Second, func() bool { return h.herdr.listAgentsCallCount() >= 1 })
	base := h.herdr.listAgentsCallCount()
	h.daemon.processLLMRetries(ctx)

	if n := h.herdr.listAgentsCallCount() - base; n != 0 {
		t.Errorf("retry must not resolve the pane while a consult is in flight; ListAgents called %d extra time(s)", n)
	}
	if q, _ := h.raw.UnprocessedLLMRetries(ctx); len(q) != 0 {
		t.Errorf("a skipped retry should still be drained, got %+v", q)
	}
	if len(h.herdr.sentInputs()) != 0 {
		t.Fatal("a guarded retry must not send anything")
	}
	if original, _ := h.raw.GetAudit(ctx, id); original == nil || original.Status != "escalated" {
		t.Errorf("a guarded retry must leave its source escalation pending, got %+v", original)
	}

	// Once the consult resolves, a fresh retry is allowed to resolve the pane.
	if err := h.raw.UpdateLLMRequestStatus(ctx, "req-agent-busy-1", "done"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.raw.InsertLLMRetry(ctx, id, time.Now()); err != nil {
		t.Fatal(err)
	}
	base = h.herdr.listAgentsCallCount()
	h.daemon.processLLMRetries(ctx)
	if n := h.herdr.listAgentsCallCount() - base; n == 0 {
		t.Error("after the consult resolved, the retry should resolve the agent's pane")
	}
	if original, _ := h.raw.GetAudit(ctx, id); original == nil || original.Status != "retried" {
		t.Errorf("accepted retry must retire its source escalation, got %+v", original)
	}
}

func TestRetryLLMTransientPendingCheckFailureStaysQueued(t *testing.T) {
	h := newHarness(t, "")
	ctx := context.Background()
	id, err := h.raw.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "agent-transient", Signature: "sig", Trigger: "agent-status: blocked",
		SituationType: domain.SituationApproval, Action: "escalated",
		Rationale: "[llm_timeout] llm timeout", Status: "escalated", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.raw.InsertLLMRetry(ctx, id, time.Now()); err != nil {
		t.Fatal(err)
	}

	h.store.(*failingStore).setFailPending(true)
	h.daemon.processLLMRetries(ctx)
	if q, _ := h.raw.UnprocessedLLMRetries(ctx); len(q) != 1 || q[0].AuditID != id {
		t.Errorf("transient pending-check failure must preserve the queued retry, got %+v", q)
	}
	if original, _ := h.raw.GetAudit(ctx, id); original == nil || original.Status != "escalated" {
		t.Errorf("transient retry failure must leave its source pending, got %+v", original)
	}
}

func TestRetryLLMAgentGoneNotifies(t *testing.T) {
	// A retry for an agent that is no longer present notifies the operator and
	// takes no action, but is still drained.
	h := newHarness(t, "")
	ctx := context.Background()
	id, _ := h.raw.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "agent-ghost", Signature: "sig", Trigger: "agent-status: blocked",
		SituationType: domain.SituationApproval, Action: "escalated",
		Rationale: "[llm_no_submit] llm exited without submitting", Status: "escalated", CreatedAt: time.Now(),
	})
	// No live agents (setAgents left empty) and no pending consult.
	if _, err := h.raw.InsertLLMRetry(ctx, id, time.Now()); err != nil {
		t.Fatal(err)
	}
	before := len(h.herdr.notified())
	h.daemon.processLLMRetries(ctx)

	if n := h.herdr.listAgentsCallCount(); n == 0 {
		t.Fatal("retry should attempt to resolve the (now absent) agent's pane")
	}
	if len(h.herdr.notified()) <= before {
		t.Error("a retry for a vanished agent should notify the operator")
	}
	if q, _ := h.raw.UnprocessedLLMRetries(ctx); len(q) != 0 {
		t.Errorf("a retry for a gone agent should still be drained, got %+v", q)
	}
	if len(h.herdr.sentInputs()) != 0 {
		t.Fatal("no send for a vanished agent")
	}
	if original, _ := h.raw.GetAudit(ctx, id); original == nil || original.Status != "escalated" {
		t.Errorf("a retry for a vanished agent was not accepted and must stay pending, got %+v", original)
	}
}

// TestRetryLLMRefreshesLiveHerdrStatus proves the retry re-drives with herdr's
// CURRENT status/type from `agent list`, not a fabricated "blocked". The
// original escalation was recorded while herdr reported "blocked"; by retry
// time the agent has moved on and herdr reports "done" with type "claude". The
// re-driven escalation must render that live status (and carry the live agent
// type), so a stale/fabricated status never leaks onto a retry.
func TestRetryLLMRefreshesLiveHerdrStatus(t *testing.T) {
	// No [llm] section → llm.configured is false, so the re-driven capture
	// escalates deterministically (no consult branch) and we can assert the
	// resulting trigger.
	h := newHarness(t, "")
	ctx := context.Background()

	// Seed a retryable LLM escalation captured earlier at status "blocked".
	seed, err := h.raw.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "agent-r", Signature: "sig", Trigger: "agent-status: blocked",
		SituationType: domain.SituationApproval, Action: "escalated",
		Rationale: "[llm_timeout] llm timeout", Status: "escalated", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Live herdr snapshot at retry time: the agent has moved on to "done" and
	// its type is known. A pane is present for the re-classification read.
	h.herdr.setPane(approvalPane)
	h.herdr.setAgents([]domain.AgentTransition{
		{AgentID: "agent-r", PaneID: "agent-r", AgentType: "claude", Status: "done"},
	})

	if _, err := h.raw.InsertLLMRetry(ctx, seed, time.Now()); err != nil {
		t.Fatal(err)
	}
	h.daemon.processLLMRetries(ctx)

	// The retry re-drives capture→classify→escalate on the live status. A new
	// escalation appears whose trigger reflects herdr's REAL status ("done"),
	// never the fabricated "blocked". Using "blocked" here (as the old code
	// did) would have produced another "agent-status: blocked" row instead.
	var redriven domain.AuditRecord
	waitFor(t, 5*time.Second, func() bool {
		esc, _ := h.raw.PendingEscalations(ctx)
		for _, e := range esc {
			if e.ID != seed && e.AgentID == "agent-r" {
				redriven = e
				return true
			}
		}
		return false
	})
	if redriven.Trigger != "agent-status: done" {
		t.Errorf("re-driven escalation trigger = %q, want %q (herdr's live status)",
			redriven.Trigger, "agent-status: done")
	}
	// The live agent type must propagate too (empty type would fall back to
	// "unknown" and break Claude's structural detectors / signature lookup).
	if redriven.AgentType != "claude" {
		t.Errorf("re-driven escalation agent type = %q, want \"claude\" (live from agent list)",
			redriven.AgentType)
	}
	if original, _ := h.raw.GetAudit(ctx, seed); original == nil || original.Status != "retried" {
		t.Errorf("re-driven retry must retire its source escalation, got %+v", original)
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
		AgentSessionID: "ba9a6f5a-ca6a-49dc-bcec-d4869ba97851",
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
		"workspace_id":     "w2",
		"tab_id":           "w2:t3",
		"pane_id":          "w2:p7",
		"agent_id":         "w2:p7",
		"agent_session_id": "ba9a6f5a-ca6a-49dc-bcec-d4869ba97851",
		"cwd":              "/home/op/project",
		"foreground_cwd":   "/home/op/project/sub",
	} {
		if got, _ := m[key].(string); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	// agent_name carries the agent's resolved short name (for {agent_name}).
	wantName, _ := h.raw.EnsureAgentName(context.Background(), "w2:p7")
	if got, _ := m["agent_name"].(string); got == "" || got != wantName {
		t.Errorf("agent_name = %q, want resolved short name %q", got, wantName)
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

func TestTruncateTailRunesKeepsBottomContext(t *testing.T) {
	if got := truncateTailRunes("short", 10); got != "short" {
		t.Errorf("short input must pass through, got %q", got)
	}
	if got := truncateTailRunes("abcdef", 3); got != "…def" {
		t.Errorf("tail cut = %q, want %q", got, "…def")
	}
	if got := truncateTailRunes("top界界bottom", 8); got != "…界界bottom" {
		t.Errorf("rune-safe tail cut = %q, want %q", got, "…界界bottom")
	}
}

func TestStoredSituationSnapshotsKeepBottomContext(t *testing.T) {
	h := newHarness(t, "")
	pane := "TOP-SCROLLBACK-MARKER\n" + strings.Repeat("old shell output 界\n", 400) + approvalPane
	h.herdr.setPane(pane)

	h.push("agent-tail-snapshot", "blocked")
	ctx := context.Background()
	var current domain.AuditRecord
	waitFor(t, 3*time.Second, func() bool {
		escalations, _ := h.raw.PendingEscalations(ctx)
		if len(escalations) == 0 {
			return false
		}
		current = escalations[0]
		return true
	})

	original, err := h.raw.GetSignatureSnapshot(ctx, current.Signature)
	if err != nil {
		t.Fatalf("original situation: %v", err)
	}
	for label, got := range map[string]string{
		"Current situation":  current.PaneExcerpt,
		"Original situation": original,
	} {
		if !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, approvalPane) {
			t.Errorf("%s must retain the bottom of oversized context, got %q", label, got)
		}
		if strings.Contains(got, "TOP-SCROLLBACK-MARKER") {
			t.Errorf("%s retained the top of oversized context", label)
		}
		if n := len([]rune(got)); n != snapshotMaxRunes+1 {
			t.Errorf("%s length = %d runes, want %d including marker", label, n, snapshotMaxRunes+1)
		}
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
