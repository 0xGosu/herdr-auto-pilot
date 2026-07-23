package frontend_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/cli"
	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/control"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
	"github.com/0xGosu/herdr-auto-pilot/internal/testutil"
)

func testApp(t *testing.T) (*frontend.App, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &frontend.App{
		Store:      st,
		ConfigPath: filepath.Join(dir, "config.toml"),
		Author:     "operator",
		// no ControlPath: nudges are skipped (daemon absent is legal)
	}, st
}

// fakeEmbedder is a canned embedder for the standalone re-embed path.
type fakeEmbedder struct {
	fail bool
	dims int
	id   string
}

func (f *fakeEmbedder) EmbedText(context.Context, string) ([]float32, error) {
	if f.fail {
		return nil, errors.New("induced embed failure")
	}
	v := make([]float32, f.dims)
	v[0] = 1
	return v, nil
}
func (f *fakeEmbedder) ModelID() string { return f.id }
func (f *fakeEmbedder) Dims() int       { return f.dims }
func (f *fakeEmbedder) Close() error    { return nil }

// seedEmbeddingRow persists one semantic identity row minted by `model`.
func seedEmbeddingRow(t *testing.T, st *store.Store, sig, model string, vec []float32) {
	t.Helper()
	if err := st.UpsertSignatureEmbedding(context.Background(), domain.SignatureEmbedding{
		Signature: sig, SituationType: domain.SituationApproval, AgentType: "claude",
		Model: model, Dims: len(vec), Vector: vec,
		Salient: "permission:" + sig, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
}

// writeEmbeddingConfig points [embedding] model_path at a real temp file so
// the drift check sees the model as present.
func writeEmbeddingConfig(t *testing.T, app *frontend.App) string {
	t.Helper()
	modelPath := filepath.Join(t.TempDir(), "test-model.gguf")
	if err := os.WriteFile(modelPath, []byte("gguf"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgTOML := fmt.Sprintf("[embedding]\nmodel_path = %q\n", modelPath)
	if err := os.WriteFile(app.ConfigPath, []byte(cfgTOML), 0o600); err != nil {
		t.Fatal(err)
	}
	return modelPath
}

func TestEmbeddingDrift(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	writeEmbeddingConfig(t, app)

	// No rows yet: no drift.
	d, err := app.EmbeddingDrift(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if d.Detected || d.ModelMissing || d.ModelID != "test-model.gguf" {
		t.Errorf("empty store must not drift: %+v", d)
	}

	seedEmbeddingRow(t, st, "current", "test-model.gguf", []float32{1, 0, 0})
	seedEmbeddingRow(t, st, "legacy", "old-model.gguf", []float32{1, 0})
	d, err = app.EmbeddingDrift(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Detected || d.Stale != 1 || d.Total != 2 {
		t.Errorf("drift = %+v, want Detected with 1 of 2 stale", d)
	}

	// GetStatus carries the same check.
	st2, err := app.GetStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !st2.Drift.Detected || st2.Drift.Stale != 1 {
		t.Errorf("status drift = %+v, want detected", st2.Drift)
	}

	// Missing model file → ModelMissing, drift still counted.
	cfgTOML := "[embedding]\nmodel_path = \"/nonexistent/other-model.gguf\"\n"
	if err := os.WriteFile(app.ConfigPath, []byte(cfgTOML), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err = app.EmbeddingDrift(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !d.ModelMissing || !d.Detected || d.Stale != 2 {
		t.Errorf("missing model: %+v, want ModelMissing with 2 stale", d)
	}

	// Disabled embedding → zero-valued.
	if err := os.WriteFile(app.ConfigPath, []byte("[embedding]\ndisabled = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err = app.EmbeddingDrift(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if d.Detected || d.ModelID != "" {
		t.Errorf("disabled embedding must report zero drift: %+v", d)
	}
}

func TestRequestReembedRequiresDaemon(t *testing.T) {
	app, _ := testApp(t)
	ctx := context.Background()

	app.DaemonInfo = func() (bool, int, string) { return false, 0, "" }
	err := app.RequestReembed(ctx)
	if err == nil || !strings.Contains(err.Error(), "hap signatures reembed") {
		t.Errorf("daemon-down error must point at the CLI remedy, got %v", err)
	}

	// Daemon up: the KindReembed nudge reaches the control socket.
	sock := filepath.Join(testutil.SocketDir(t), "ctl.sock")
	var mu sync.Mutex
	var kinds []control.Kind
	srv, err := control.NewServer(sock, func(k control.Kind) {
		mu.Lock()
		kinds = append(kinds, k)
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	app.ControlPath = sock
	app.DaemonInfo = func() (bool, int, string) { return true, 42, "test" }
	if err := app.RequestReembed(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(kinds)
		mu.Unlock()
		if n > 0 {
			if kinds[0] != control.KindReembed {
				t.Errorf("nudge kind = %v, want %v", kinds[0], control.KindReembed)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("reembed nudge never reached the daemon socket")
}

func TestReembedStandalone(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	writeEmbeddingConfig(t, app)
	seedEmbeddingRow(t, st, "legacy", "old-model.gguf", []float32{1, 0})
	seedEmbeddingRow(t, st, "current", "test-model.gguf", []float32{1, 0, 0})
	app.NewEmbedder = func(config.Embedding) ports.EmbedderPort {
		return &fakeEmbedder{dims: 3, id: "test-model.gguf"}
	}

	// Refused while a daemon runs (it owns signature_embeddings writes).
	app.DaemonInfo = func() (bool, int, string) { return true, 42, "test" }
	if _, err := app.ReembedStandalone(ctx, nil); err == nil {
		t.Fatal("standalone re-embed must refuse while a daemon is running")
	}

	app.DaemonInfo = func() (bool, int, string) { return false, 0, "" }
	res, err := app.ReembedStandalone(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Reembedded != 1 || res.Kept != 1 || res.Downgraded != 0 {
		t.Errorf("Reembedded/Kept/Downgraded = %d/%d/%d, want 1/1/0",
			res.Reembedded, res.Kept, res.Downgraded)
	}
	d, err := app.EmbeddingDrift(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if d.Detected {
		t.Errorf("drift must clear after standalone re-embed: %+v", d)
	}

	// An unavailable model fails loudly instead of silently doing nothing.
	app.NewEmbedder = func(config.Embedding) ports.EmbedderPort {
		return &fakeEmbedder{fail: true}
	}
	seedEmbeddingRow(t, st, "legacy2", "old-model.gguf", []float32{1, 0})
	if _, err := app.ReembedStandalone(ctx, nil); err == nil ||
		!strings.Contains(err.Error(), "embedding model unavailable") {
		t.Errorf("warm failure must surface, got %v", err)
	}

	// Disabled embedding is an explicit error, not a silent no-op.
	if err := os.WriteFile(app.ConfigPath, []byte("[embedding]\ndisabled = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := app.ReembedStandalone(ctx, nil); err == nil {
		t.Error("disabled embedding must error")
	}
}

func TestPauseResumeAppendsKillEvents(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()

	if err := app.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	stat, err := app.GetStatus(ctx)
	if err != nil || !stat.Paused {
		t.Fatalf("pause not reflected: %+v %v", stat, err)
	}

	if err := app.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	stat, _ = app.GetStatus(ctx)
	if stat.Paused {
		t.Fatal("resume not reflected")
	}

	// Full history retained (append-only, FR-017).
	events, _ := st.KillEvents(ctx, 10)
	if len(events) != 2 {
		t.Errorf("kill history rows = %d, want 2", len(events))
	}
}

func TestResolveRecordsCorrection(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	id, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "a1", Signature: "sig", Trigger: "t",
		SituationType: domain.SituationApproval, Action: "escalated",
		Status: "escalated", Suggestion: "respond: y", CreatedAt: time.Now(),
	})

	if err := app.Resolve(ctx, id, "n", false); err != nil {
		t.Fatal(err)
	}
	corr, _ := st.UnprocessedCorrections(ctx)
	if len(corr) != 1 || corr[0].CorrectedAction != "n" || corr[0].AuditID != id {
		t.Errorf("correction not recorded: %+v", corr)
	}
}

func TestConfirmUsesSuggestion(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	id, _ := st.AppendAudit(ctx, domain.AuditRecord{
		SituationType: domain.SituationChoice, Trigger: "t", Action: "escalated",
		Status: "escalated", Suggestion: "choose: pnpm", CreatedAt: time.Now(),
	})
	if err := app.Confirm(ctx, id, false); err != nil {
		t.Fatal(err)
	}
	corr, _ := st.UnprocessedCorrections(ctx)
	if len(corr) != 1 || corr[0].CorrectedAction != "pnpm" {
		t.Errorf("confirm should record the suggested action: %+v", corr)
	}
}

// fakeHerdr captures Send calls for confirm/resolve delivery assertions.
var errAny = errors.New("induced failure")

type fakeHerdr struct {
	panes   []string
	inputs  []string
	pane    string // returned by ReadPane (live menu content)
	readErr error
	sendErr error                    // when set, Send fails (delivery failure)
	agents  []domain.AgentTransition // returned by ListAgents (live statuses)
}

func (f *fakeHerdr) Send(_ context.Context, paneID, input string) error {
	if f.sendErr != nil {
		return f.sendErr
	}
	f.panes = append(f.panes, paneID)
	f.inputs = append(f.inputs, input)
	return nil
}

func (f *fakeHerdr) ReadPane(context.Context, string, int) (string, error) {
	return f.pane, f.readErr
}

func (f *fakeHerdr) ListAgents(context.Context) ([]domain.AgentTransition, error) {
	return f.agents, nil
}

// TestResolveMarksSentOnDelivery: a delivered correction is recorded with
// Sent=true so the daemon arms the post-action unblock self-check.
func TestResolveMarksSentOnDelivery(t *testing.T) {
	app, st := testApp(t)
	fake := &fakeHerdr{pane: "Do you want to proceed?\n❯ 1. Yes\n  2. No\n"}
	app.Herdr = fake
	ctx := context.Background()
	id, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "a1", SituationType: domain.SituationApproval, Trigger: "t",
		Action: "escalated", Status: "escalated", Suggestion: "respond: y", CreatedAt: time.Now(),
	})
	if err := app.Resolve(ctx, id, "Yes", true); err != nil {
		t.Fatal(err)
	}
	corr, _ := st.UnprocessedCorrections(ctx)
	if len(corr) != 1 || !corr[0].Sent {
		t.Errorf("delivered correction should be Sent=true: %+v", corr)
	}
	if len(fake.inputs) != 1 {
		t.Errorf("expected one delivery, got %v", fake.inputs)
	}
}

func TestConfirmCodexRateLimitErrorSendsSelectedMenuDigit(t *testing.T) {
	app, st := testApp(t)
	pane := "Approaching rate limits\n" +
		"Switch to gpt-5.4-mini for lower credit usage?\n\n" +
		"› 1. Switch to gpt-5.4-mini                 Small, fast, and cost-efficient model for simpler coding tasks.\n" +
		"  2. Keep current model\n" +
		"  3. Keep current model (never show again)  Hide future rate limit reminders about switching models.\n\n" +
		"Press enter to confirm or esc to go back\n"
	fake := &fakeHerdr{pane: pane}
	app.Herdr = fake
	ctx := context.Background()
	id, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "codex-pane", AgentType: "codex", SituationType: domain.SituationError,
		Trigger: "agent-status: idle", Action: "escalated", Status: "escalated",
		Suggestion: "on error: Keep current model", PaneExcerpt: pane, CreatedAt: time.Now(),
	})

	if err := app.Confirm(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	if len(fake.inputs) != 1 || fake.inputs[0] != "2" {
		t.Fatalf("Codex rate-limit confirmation sent %v, want menu digit 2", fake.inputs)
	}
	corr, _ := st.UnprocessedCorrections(ctx)
	if len(corr) != 1 || corr[0].CorrectedAction != "Keep current model" || !corr[0].Sent {
		t.Fatalf("rate-limit confirmation correction = %+v", corr)
	}
}

func TestConfirmNonCodexRateLimitShapedErrorStaysLiteral(t *testing.T) {
	app, st := testApp(t)
	pane := "Approaching rate limits\n" +
		"Switch to gpt-5.4-mini for lower credit usage?\n\n" +
		"› 1. Switch to gpt-5.4-mini                 Small, fast, and cost-efficient model for simpler coding tasks.\n" +
		"  2. Keep current model\n" +
		"  3. Keep current model (never show again)  Hide future rate limit reminders about switching models.\n\n" +
		"Press enter to confirm or esc to go back\n"
	fake := &fakeHerdr{pane: pane}
	app.Herdr = fake
	ctx := context.Background()
	id, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "claude-pane", AgentType: "claude", SituationType: domain.SituationError,
		Trigger: "agent-status: blocked", Action: "escalated", Status: "escalated",
		Suggestion: "on error: Keep current model", PaneExcerpt: pane, CreatedAt: time.Now(),
	})

	if err := app.Confirm(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	if len(fake.inputs) != 1 || fake.inputs[0] != "Keep current model" {
		t.Fatalf("non-Codex rate-limit-shaped error sent %v, want literal reply", fake.inputs)
	}
}

// TestResolveRecordOnlyNotSent: a record-only correction (no --send) leaves
// Sent=false so the daemon does NOT run the self-check on an expectedly-blocked
// agent.
func TestResolveRecordOnlyNotSent(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	id, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "a1", SituationType: domain.SituationApproval, Trigger: "t",
		Action: "escalated", Status: "escalated", Suggestion: "respond: y", CreatedAt: time.Now(),
	})
	if err := app.Resolve(ctx, id, "n", false); err != nil {
		t.Fatal(err)
	}
	corr, _ := st.UnprocessedCorrections(ctx)
	if len(corr) != 1 || corr[0].Sent {
		t.Errorf("record-only correction should be Sent=false: %+v", corr)
	}
}

// TestResolveNoopNeverSent: a @noop resolution sends nothing and is Sent=false
// even with --send.
func TestResolveNoopNeverSent(t *testing.T) {
	app, st := testApp(t)
	fake := &fakeHerdr{}
	app.Herdr = fake
	ctx := context.Background()
	id, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "a1", SituationType: domain.SituationApproval, Trigger: "t",
		Action: "escalated", Status: "escalated", Suggestion: "respond: y", CreatedAt: time.Now(),
	})
	if err := app.Resolve(ctx, id, "@noop", true); err != nil {
		t.Fatal(err)
	}
	corr, _ := st.UnprocessedCorrections(ctx)
	if len(corr) != 1 || corr[0].Sent {
		t.Errorf("@noop correction must be Sent=false: %+v", corr)
	}
	if len(fake.inputs) != 0 {
		t.Errorf("@noop must not deliver anything, got %v", fake.inputs)
	}
}

// TestResolveFailedSendLeavesUnsent: when delivery fails, the correction is
// still recorded (learning) but stays Sent=false, so the daemon never arms the
// self-check for an action that never reached the agent.
func TestResolveFailedSendLeavesUnsent(t *testing.T) {
	app, st := testApp(t)
	fake := &fakeHerdr{pane: "Enter a commit message:\n> ", sendErr: errAny}
	app.Herdr = fake
	ctx := context.Background()
	id, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "a1", SituationType: domain.SituationApproval, Trigger: "t",
		Action: "escalated", Status: "escalated", Suggestion: "respond: y", CreatedAt: time.Now(),
	})
	if err := app.Resolve(ctx, id, "proceed", true); err == nil {
		t.Fatal("expected a delivery error")
	}
	corr, _ := st.UnprocessedCorrections(ctx)
	if len(corr) != 1 {
		t.Fatalf("correction must still be recorded on send failure: %+v", corr)
	}
	if corr[0].Sent {
		t.Errorf("a failed send must leave the correction Sent=false: %+v", corr)
	}
}

func TestConfirmSendsRenderedDeclaredTaskPrompt(t *testing.T) {
	// The confirm path must deliver the exact rendered prompt carried in the
	// suggestion (the SuggestedAction / materializeForSend contract), while
	// recording the symbolic action. An LLM task review wraps that suggestion in
	// "LLM suggested: ", so both layers must be peeled — matching the task-send
	// prefix against the unpeeled string would miss and type the raw
	// "@next_task:declared" sentinel into the agent's pane.
	prompt := domain.DeclaredTask{Task: "step two", Path: "/docs/tasks.md"}.Prompt()
	for _, tc := range []struct {
		name       string
		suggestion string
		want       string
	}{
		{"declared", "send next declared task: " + prompt, domain.ActionNextDeclaredTask},
		{"inferred", "send inferred next task: " + prompt, domain.ActionNextInferredTask},
		{"llm-reviewed declared", "LLM suggested: send next declared task: " + prompt, domain.ActionNextDeclaredTask},
		{"llm-reviewed inferred", "LLM suggested: send inferred next task: " + prompt, domain.ActionNextInferredTask},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, st := testApp(t)
			fake := &fakeHerdr{}
			app.Herdr = fake
			ctx := context.Background()

			id, _ := st.AppendAudit(ctx, domain.AuditRecord{
				AgentID: "w1:p1", SituationType: domain.SituationIdle, Trigger: "t",
				Action: "escalated", Status: "escalated",
				Suggestion: tc.suggestion, CreatedAt: time.Now(),
			})
			if err := app.Confirm(ctx, id, true); err != nil {
				t.Fatal(err)
			}
			corr, _ := st.UnprocessedCorrections(ctx)
			if len(corr) != 1 || corr[0].CorrectedAction != tc.want {
				t.Errorf("confirm should record the symbolic action %q: %+v", tc.want, corr)
			}
			if len(fake.inputs) != 1 || fake.inputs[0] != prompt {
				t.Errorf("delivered %v, want the rendered prompt %q", fake.inputs, prompt)
			}
			if len(fake.panes) != 1 || fake.panes[0] != "w1:p1" {
				t.Errorf("delivered to %v, want the audit's agent pane", fake.panes)
			}
		})
	}
}

func TestConfirmGeneratedTaskWritesSourceAndSends(t *testing.T) {
	// Confirming an idle task suggestion writes a per-agent tasks.md (single
	// in-progress "[-]" item), registers a matching [[task_sources]] entry,
	// records the correction, and sends the task to the agent.
	app, st := testApp(t)
	fake := &fakeHerdr{}
	app.Herdr = fake
	// Route the tasks file into a known state dir.
	stateDir := t.TempDir()
	app.StateDir = stateDir
	ctx := context.Background()

	name, _ := st.EnsureAgentName(ctx, "w1:p1")
	taskText := "Investigate the flaky auth test and add a retry guard"
	id, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w1:p1", SituationType: domain.SituationIdle, Trigger: "t",
		Action: "escalated", Status: "escalated",
		Suggestion: domain.SuggestTaskPrefix + taskText, CreatedAt: time.Now(),
	})

	if err := app.Confirm(ctx, id, true); err != nil {
		t.Fatal(err)
	}

	// tasks.md written, and the delivered item reserved "[-]" (the marker is
	// applied at delivery time, not at file-creation time — issue #156).
	path := filepath.Join(stateDir, "tasks", name+".md")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("tasks file not written: %v", err)
	}
	if !strings.Contains(string(body), "- [-] 1. "+taskText) {
		t.Errorf("tasks file = %q, want a single numbered in-progress %q item", body, taskText)
	}

	// The item is parsed as not-actionable, so the declared-task resolver
	// treats the list as complete (no next "[ ]" item to re-send).
	if next := domain.NextDeclaredTask(string(body)); next != "" {
		t.Errorf("in-progress item must not resolve as a next declared task, got %q", next)
	}

	// A matching task source was appended, scoped to the agent, pointing at
	// the absolute file path.
	cfg, err := config.Load(app.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.TaskSources) != 1 {
		t.Fatalf("want 1 task source, got %d", len(cfg.TaskSources))
	}
	if cfg.TaskSources[0].Agent != name || cfg.TaskSources[0].Path != path {
		t.Errorf("task source = %+v, want agent %q path %q", cfg.TaskSources[0], name, path)
	}

	// The correction resolves the escalation and learns the declared-task
	// action.
	corr, _ := st.UnprocessedCorrections(ctx)
	if len(corr) != 1 || corr[0].CorrectedAction != domain.ActionNextDeclaredTask || corr[0].AuditID != id {
		t.Errorf("confirm should record a declared-task correction: %+v", corr)
	}

	// The generated task uses the same default prompt as every declared task,
	// including the newly created task-list path.
	wantPrompt := domain.DeclaredTask{Task: taskText, Path: path, AgentName: name}.Prompt()
	if len(fake.inputs) != 1 || fake.inputs[0] != wantPrompt {
		t.Errorf("delivered %v, want the rendered prompt %q", fake.inputs, wantPrompt)
	}
	if len(fake.panes) != 1 || fake.panes[0] != "w1:p1" {
		t.Errorf("delivered to %v, want the audit's agent pane", fake.panes)
	}
}

func TestConfirmGeneratedTaskWithoutSendStillWritesSource(t *testing.T) {
	// send=false establishes the source and file but delivers nothing — and
	// must leave the first item "[ ]" so the daemon's idle flow can hand it
	// out later. Regression for issue #156: the item used to be pre-marked
	// "[-]" at write time, which suppressed the idle resend forever.
	app, st := testApp(t)
	fake := &fakeHerdr{}
	app.Herdr = fake
	stateDir := t.TempDir()
	app.StateDir = stateDir
	ctx := context.Background()

	name, _ := st.EnsureAgentName(ctx, "w2:p2")
	id, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w2:p2", SituationType: domain.SituationIdle, Trigger: "t",
		Action: "escalated", Status: "escalated",
		Suggestion: domain.SuggestTaskPrefix + "Write missing tests", CreatedAt: time.Now(),
	})
	if err := app.Confirm(ctx, id, false); err != nil {
		t.Fatal(err)
	}
	if len(fake.inputs) != 0 {
		t.Errorf("send=false must deliver nothing, got %v", fake.inputs)
	}
	cfg, _ := config.Load(app.ConfigPath)
	if len(cfg.TaskSources) != 1 {
		t.Errorf("source must still be registered on a non-send confirm, got %d", len(cfg.TaskSources))
	}
	body, err := os.ReadFile(filepath.Join(stateDir, "tasks", name+".md"))
	if err != nil {
		t.Fatalf("tasks file not written: %v", err)
	}
	if !strings.Contains(string(body), "- [ ] 1. Write missing tests") {
		t.Errorf("tasks file = %q, want the undelivered item pending \"[ ]\"", body)
	}
	if next := domain.NextDeclaredTask(string(body)); next != "1. Write missing tests" {
		t.Errorf("next declared task = %q, want the undelivered first item — a stranded item would never be sent", next)
	}
}

func TestConfirmGeneratedMultipleTasksWritesChecklist(t *testing.T) {
	// A multiline suggestion (a Markdown checklist from the LLM) is normalized:
	// ONLY the first task is sent to the agent, so after the send it reads
	// in-progress "[-]" (reserved at delivery) and the rest stay pending "[ ]".
	app, st := testApp(t)
	fake := &fakeHerdr{}
	app.Herdr = fake
	stateDir := t.TempDir()
	app.StateDir = stateDir
	ctx := context.Background()

	name, _ := st.EnsureAgentName(ctx, "w1:p1")
	// The model already used Markdown checkboxes — markers must be stripped,
	// not double-inserted.
	suggestion := domain.SuggestTaskPrefix + "- [ ] Investigate the flaky auth test\n- [ ] Add a retry guard\n- [ ] Backfill unit tests"
	id, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w1:p1", SituationType: domain.SituationIdle, Trigger: "t",
		Action: "escalated", Status: "escalated",
		Suggestion: suggestion, CreatedAt: time.Now(),
	})

	if err := app.Confirm(ctx, id, true); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(stateDir, "tasks", name+".md")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("tasks file not written: %v", err)
	}
	want := "- [-] 1. Investigate the flaky auth test\n- [ ] 2. Add a retry guard\n- [ ] 3. Backfill unit tests\n"
	if !strings.Contains(string(body), want) {
		t.Errorf("tasks file = %q, want a numbered checklist %q", body, want)
	}
	// Only the first task is sent to the agent, rendered through the same
	// default prompt used for later items from the registered source. The
	// first task is sent from the raw normalized suggestion (never re-read
	// from the numbered file), so it stays clean, unnumbered text.
	wantPrompt := domain.DeclaredTask{
		Task: "Investigate the flaky auth test", Path: path, AgentName: name,
	}.Prompt()
	if len(fake.inputs) != 1 || fake.inputs[0] != wantPrompt {
		t.Errorf("delivered %v, want only the first task as %q", fake.inputs, wantPrompt)
	}
	// The next declared task is the first pending item, so the queue drives on
	// later idles. Its numbered ID marker is NOT stripped when read back — it
	// is sent to the agent as part of the task text, same as any hand-authored
	// numbered checklist item.
	if next := domain.NextDeclaredTask(string(body)); next != "2. Add a retry guard" {
		t.Errorf("next declared task = %q, want the first pending item with its ID marker intact", next)
	}
}

func TestConfirmGeneratedTaskIsIdempotent(t *testing.T) {
	// A double-submit (or re-confirm after resolution) must not re-send the
	// task or accumulate duplicate task sources: the atomic claim lets only the
	// first confirm apply side effects.
	app, st := testApp(t)
	fake := &fakeHerdr{}
	app.Herdr = fake
	app.StateDir = t.TempDir()
	ctx := context.Background()

	id, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w3:p3", SituationType: domain.SituationIdle, Trigger: "t",
		Action: "escalated", Status: "escalated",
		Suggestion: domain.SuggestTaskPrefix + "Do the thing", CreatedAt: time.Now(),
	})

	if err := app.Confirm(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	// Second confirm must fail (already claimed) and change nothing.
	if err := app.Confirm(ctx, id, true); err == nil {
		t.Error("second confirm on a resolved escalation must fail")
	}

	if len(fake.inputs) != 1 {
		t.Errorf("task must be sent exactly once, got %d sends", len(fake.inputs))
	}
	cfg, _ := config.Load(app.ConfigPath)
	if len(cfg.TaskSources) != 1 {
		t.Errorf("want exactly 1 task source after a double confirm, got %d", len(cfg.TaskSources))
	}
}

func TestConfirmGeneratedTaskSendFailureRollsBackToPending(t *testing.T) {
	// A failed --send delivery must roll the reserved item back to "[ ]" so
	// the daemon's idle flow can retry it — mirroring SendTaskToAgent. Before
	// issue #156 the item stayed "[-]" and was stranded forever.
	app, st := testApp(t)
	fake := &fakeHerdr{sendErr: errors.New("pane vanished")}
	app.Herdr = fake
	stateDir := t.TempDir()
	app.StateDir = stateDir
	ctx := context.Background()

	name, _ := st.EnsureAgentName(ctx, "w5:p5")
	id, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w5:p5", SituationType: domain.SituationIdle, Trigger: "t",
		Action: "escalated", Status: "escalated",
		Suggestion: domain.SuggestTaskPrefix + "Fix the flaky login test", CreatedAt: time.Now(),
	})
	if err := app.Confirm(ctx, id, true); err == nil {
		t.Fatal("confirm must surface the failed delivery")
	}
	body, err := os.ReadFile(filepath.Join(stateDir, "tasks", name+".md"))
	if err != nil {
		t.Fatalf("tasks file not written: %v", err)
	}
	if !strings.Contains(string(body), "- [ ] 1. Fix the flaky login test") {
		t.Errorf("tasks file = %q, want the failed-send item rolled back to \"[ ]\"", body)
	}
	if next := domain.NextDeclaredTask(string(body)); next != "1. Fix the flaky login test" {
		t.Errorf("next declared task = %q, want the rolled-back item so the idle flow retries it", next)
	}
}

func TestConfirmRepeatedGenerationPreservesMarkers(t *testing.T) {
	// A later generation escalation carrying the SAME tasks (e.g. a stale
	// duplicate raised before the first confirm registered the source) must
	// not rewrite the file: resetting a delivered item's "[-]" back to "[ ]"
	// would re-arm the daemon to send it a second time.
	app, st := testApp(t)
	fake := &fakeHerdr{}
	app.Herdr = fake
	stateDir := t.TempDir()
	app.StateDir = stateDir
	ctx := context.Background()

	name, _ := st.EnsureAgentName(ctx, "w6:p6")
	suggestion := domain.SuggestTaskPrefix + "Profile the slow endpoint"
	first, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w6:p6", SituationType: domain.SituationIdle, Trigger: "t",
		Action: "escalated", Status: "escalated",
		Suggestion: suggestion, CreatedAt: time.Now(),
	})
	if err := app.Confirm(ctx, first, true); err != nil {
		t.Fatal(err)
	}

	second, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w6:p6", SituationType: domain.SituationIdle, Trigger: "t",
		Action: "escalated", Status: "escalated",
		Suggestion: suggestion, CreatedAt: time.Now(),
	})
	if err := app.Confirm(ctx, second, false); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(filepath.Join(stateDir, "tasks", name+".md"))
	if err != nil {
		t.Fatalf("tasks file missing: %v", err)
	}
	if !strings.Contains(string(body), "- [-] 1. Profile the slow endpoint") {
		t.Errorf("tasks file = %q, want the delivered item still reserved \"[-]\" after a same-tasks re-confirm", body)
	}
	if len(fake.inputs) != 1 {
		t.Errorf("task must be delivered exactly once, got %d sends", len(fake.inputs))
	}
	cfg, _ := config.Load(app.ConfigPath)
	if len(cfg.TaskSources) != 1 {
		t.Errorf("want exactly 1 task source, got %d", len(cfg.TaskSources))
	}

	// The sharper duplicate: --send on yet another same-tasks escalation gets
	// its own successful claim (a distinct audit row), reaches the reserve —
	// and must refuse there, because the item is already "[-]". No second
	// delivery, and the reservation stands.
	third, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w6:p6", SituationType: domain.SituationIdle, Trigger: "t",
		Action: "escalated", Status: "escalated",
		Suggestion: suggestion, CreatedAt: time.Now(),
	})
	if err := app.Confirm(ctx, third, true); err == nil || !strings.Contains(err.Error(), "no longer pending") {
		t.Fatalf("a --send duplicate must refuse to re-reserve the [-] item, got %v", err)
	}
	if len(fake.inputs) != 1 {
		t.Errorf("the duplicate must not deliver again, got %d sends", len(fake.inputs))
	}
	body, _ = os.ReadFile(filepath.Join(stateDir, "tasks", name+".md"))
	if !strings.Contains(string(body), "- [-] 1. Profile the slow endpoint") {
		t.Errorf("tasks file = %q, want the reservation untouched by the refused duplicate", body)
	}
}

func TestConfirmRegenerationCarriesOverMarkers(t *testing.T) {
	// A later generation carrying a DIFFERENT task list rewrites the file, but
	// items it re-lists keep their progress markers: resetting a delivered
	// "[-]" to "[ ]" would re-arm the daemon for a duplicate send.
	app, st := testApp(t)
	fake := &fakeHerdr{}
	app.Herdr = fake
	stateDir := t.TempDir()
	app.StateDir = stateDir
	ctx := context.Background()

	name, _ := st.EnsureAgentName(ctx, "w7:p7")
	first, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w7:p7", SituationType: domain.SituationIdle, Trigger: "t",
		Action: "escalated", Status: "escalated",
		Suggestion: domain.SuggestTaskPrefix + "Profile the slow endpoint", CreatedAt: time.Now(),
	})
	if err := app.Confirm(ctx, first, true); err != nil {
		t.Fatal(err)
	}

	second, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w7:p7", SituationType: domain.SituationIdle, Trigger: "t",
		Action: "escalated", Status: "escalated",
		Suggestion: domain.SuggestTaskPrefix + "Profile the slow endpoint\nAdd a response cache", CreatedAt: time.Now(),
	})
	if err := app.Confirm(ctx, second, false); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(filepath.Join(stateDir, "tasks", name+".md"))
	if err != nil {
		t.Fatalf("tasks file missing: %v", err)
	}
	if !strings.Contains(string(body), "- [-] 1. Profile the slow endpoint") {
		t.Errorf("tasks file = %q, want the re-listed delivered item still \"[-]\"", body)
	}
	if !strings.Contains(string(body), "- [ ] 2. Add a response cache") {
		t.Errorf("tasks file = %q, want the new item appended pending", body)
	}
	if next := domain.NextDeclaredTask(string(body)); next != "2. Add a response cache" {
		t.Errorf("next declared task = %q, want only the new pending item", next)
	}
}

func TestConfirmRegenerationAppendsKeepingReservedMarker(t *testing.T) {
	// A later generation APPENDS new work and preserves the existing list in
	// place (issue #183): the already-reserved "[-]" item keeps its marker AND
	// its position, and the new task lands at the end pending — never reordered
	// ahead of it, and never reset to "[ ]" (which would re-arm the daemon for a
	// duplicate send).
	app, st := testApp(t)
	fake := &fakeHerdr{}
	app.Herdr = fake
	stateDir := t.TempDir()
	app.StateDir = stateDir
	ctx := context.Background()

	name, _ := st.EnsureAgentName(ctx, "w8:p8")
	first, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w8:p8", SituationType: domain.SituationIdle, Trigger: "t",
		Action: "escalated", Status: "escalated",
		Suggestion: domain.SuggestTaskPrefix + "Profile the slow endpoint\nAdd a response cache", CreatedAt: time.Now(),
	})
	if err := app.Confirm(ctx, first, true); err != nil {
		t.Fatal(err)
	}

	// The second suggestion lists a brand-new task first, then re-lists the two
	// existing ones — append-only keeps the existing order and appends only the
	// genuinely-new task at the end.
	second, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w8:p8", SituationType: domain.SituationIdle, Trigger: "t",
		Action: "escalated", Status: "escalated",
		Suggestion: domain.SuggestTaskPrefix + "Set up the load test rig\nProfile the slow endpoint\nAdd a response cache", CreatedAt: time.Now(),
	})
	if err := app.Confirm(ctx, second, false); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(filepath.Join(stateDir, "tasks", name+".md"))
	if err != nil {
		t.Fatalf("tasks file missing: %v", err)
	}
	if !strings.Contains(string(body), "- [-] 1. Profile the slow endpoint") {
		t.Errorf("tasks file = %q, want the delivered item still \"[-]\" in its original position", body)
	}
	if !strings.Contains(string(body), "- [ ] 2. Add a response cache") {
		t.Errorf("tasks file = %q, want the existing pending item preserved", body)
	}
	if !strings.Contains(string(body), "- [ ] 3. Set up the load test rig") {
		t.Errorf("tasks file = %q, want the new item appended pending at the end", body)
	}
	if next := domain.NextDeclaredTask(string(body)); next != "2. Add a response cache" {
		t.Errorf("next declared task = %q, want the first still-pending item — the reserved one must not be re-armed", next)
	}
}

func TestConfirmRegenerationAppendsKeepingCompletedMarker(t *testing.T) {
	// Same for FINISHED work: an item the agent completed ("[x]", e.g. via
	// `hap task done`) stays done and in place when a later generation appends
	// new work — it is never re-queued (issue #183).
	app, st := testApp(t)
	fake := &fakeHerdr{}
	app.Herdr = fake
	stateDir := t.TempDir()
	app.StateDir = stateDir
	ctx := context.Background()

	name, _ := st.EnsureAgentName(ctx, "w9:p9")
	first, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w9:p9", SituationType: domain.SituationIdle, Trigger: "t",
		Action: "escalated", Status: "escalated",
		Suggestion: domain.SuggestTaskPrefix + "Profile the slow endpoint", CreatedAt: time.Now(),
	})
	if err := app.Confirm(ctx, first, true); err != nil {
		t.Fatal(err)
	}
	// The agent finishes the delivered task ("[-]" → "[x]").
	path := filepath.Join(stateDir, "tasks", name+".md")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("tasks file missing: %v", err)
	}
	done, err := domain.SetChecklistItemDone(string(body), 1, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(done), 0o600); err != nil {
		t.Fatal(err)
	}

	second, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w9:p9", SituationType: domain.SituationIdle, Trigger: "t",
		Action: "escalated", Status: "escalated",
		Suggestion: domain.SuggestTaskPrefix + "Set up the load test rig\nProfile the slow endpoint", CreatedAt: time.Now(),
	})
	if err := app.Confirm(ctx, second, false); err != nil {
		t.Fatal(err)
	}

	body, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("tasks file missing: %v", err)
	}
	if !strings.Contains(string(body), "- [x] 1. Profile the slow endpoint") {
		t.Errorf("tasks file = %q, want the completed item still \"[x]\" in its original position", body)
	}
	if next := domain.NextDeclaredTask(string(body)); next != "2. Set up the load test rig" {
		t.Errorf("next declared task = %q, want the appended new item — completed work must not be re-queued", next)
	}
}

func TestConfirmGeneratedTaskRefusesWhenAgentWorking(t *testing.T) {
	// If the agent has started working by the time the operator confirms, the
	// suggestion is stale: no source is created, nothing is sent, and the
	// escalation stays pending so the operator can dismiss it.
	app, st := testApp(t)
	fake := &fakeHerdr{agents: []domain.AgentTransition{{AgentID: "w4:p4", Status: "working"}}}
	app.Herdr = fake
	app.StateDir = t.TempDir()
	ctx := context.Background()

	id, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w4:p4", SituationType: domain.SituationIdle, Trigger: "t",
		Action: "escalated", Status: "escalated",
		Suggestion: domain.SuggestTaskPrefix + "Do the thing", CreatedAt: time.Now(),
	})
	err := app.Confirm(ctx, id, true)
	if err == nil {
		t.Fatal("confirming a stale suggestion for a working agent must fail")
	}
	// The sentinel is the contract the TUI keys off to offer "add to list
	// instead" — a plain error would strand that fallback.
	if !errors.Is(err, frontend.ErrSuggestionStaleAgentBusy) {
		t.Errorf("send refusal must wrap ErrSuggestionStaleAgentBusy, got %v", err)
	}
	if len(fake.inputs) != 0 {
		t.Errorf("nothing may be sent to a working agent, got %v", fake.inputs)
	}
	cfg, _ := config.Load(app.ConfigPath)
	if len(cfg.TaskSources) != 0 {
		t.Errorf("no task source may be created for a stale suggestion, got %d", len(cfg.TaskSources))
	}
	// The escalation is untouched (still pending), so it can be dismissed.
	audit, _ := st.GetAudit(ctx, id)
	if audit.Status != "escalated" {
		t.Errorf("escalation must remain pending after a refused confirm, got %q", audit.Status)
	}
}

func TestConfirmGeneratedTaskAddOnlyWhileAgentWorking(t *testing.T) {
	// send=false QUEUES the tasks even while the agent is working: nothing
	// reaches the pane (so the busy agent is never interrupted), the source and
	// pending-"[ ]" file are created, the correction is recorded, and the
	// escalation is resolved (accepted). The daemon delivers the item on the
	// agent's next idle. This is the "a: add to list" path — the staleness
	// refusal only applies to a send (issue #180).
	app, st := testApp(t)
	fake := &fakeHerdr{agents: []domain.AgentTransition{{AgentID: "w5:p5", Status: "working"}}}
	app.Herdr = fake
	stateDir := t.TempDir()
	app.StateDir = stateDir
	ctx := context.Background()

	name, _ := st.EnsureAgentName(ctx, "w5:p5")
	id, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w5:p5", SituationType: domain.SituationIdle, Trigger: "t",
		Action: "escalated", Status: "escalated",
		Suggestion: domain.SuggestTaskPrefix + "Write missing tests", CreatedAt: time.Now(),
	})

	if err := app.Confirm(ctx, id, false); err != nil {
		t.Fatalf("add-only confirm must succeed for a working agent: %v", err)
	}
	// Nothing delivered to the busy agent's pane.
	if len(fake.inputs) != 0 {
		t.Errorf("add-only must deliver nothing to a working agent, got %v", fake.inputs)
	}
	// Source registered and the item left pending "[ ]" so the daemon's idle
	// flow hands it out later.
	cfg, _ := config.Load(app.ConfigPath)
	if len(cfg.TaskSources) != 1 {
		t.Fatalf("add-only must register the task source, got %d", len(cfg.TaskSources))
	}
	body, err := os.ReadFile(filepath.Join(stateDir, "tasks", name+".md"))
	if err != nil {
		t.Fatalf("tasks file not written: %v", err)
	}
	if !strings.Contains(string(body), "- [ ] 1. Write missing tests") {
		t.Errorf("tasks file = %q, want the queued item pending \"[ ]\"", body)
	}
	if next := domain.NextDeclaredTask(string(body)); next != "1. Write missing tests" {
		t.Errorf("next declared task = %q, want the queued item — a stranded item would never be sent", next)
	}
	// Accepted: the correction learns the declared-task action and the
	// escalation is resolved.
	corr, _ := st.UnprocessedCorrections(ctx)
	if len(corr) != 1 || corr[0].CorrectedAction != domain.ActionNextDeclaredTask || corr[0].AuditID != id {
		t.Errorf("add-only should record a declared-task correction: %+v", corr)
	}
	audit, _ := st.GetAudit(ctx, id)
	if audit.Status != "resolved" {
		t.Errorf("escalation must be resolved (accepted) after add-only, got %q", audit.Status)
	}
}

// declaredSourceApp builds a testApp with one declared task source for the
// named agent, seeding its checklist file with content. It returns the app,
// store, fake herdr, the agent's short name, and the absolute source path.
func declaredSourceApp(t *testing.T, agentID, content string) (*frontend.App, *store.Store, *fakeHerdr, string, string) {
	t.Helper()
	app, st := testApp(t)
	fake := &fakeHerdr{}
	app.Herdr = fake
	app.StateDir = t.TempDir()
	ctx := context.Background()
	name, _ := st.EnsureAgentName(ctx, agentID)
	path := filepath.Join(t.TempDir(), "declared.md")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := app.AddTaskSource(ctx, name, "", path, ""); err != nil {
		t.Fatal(err)
	}
	return app, st, fake, name, path
}

// generatedEscalation seeds a pending generated-task escalation for agentID.
func generatedEscalation(t *testing.T, st *store.Store, agentID, suggestion string) int64 {
	t.Helper()
	id, err := st.AppendAudit(context.Background(), domain.AuditRecord{
		AgentID: agentID, SituationType: domain.SituationIdle, Trigger: "t",
		Action: "escalated", Status: "escalated",
		Suggestion: domain.SuggestTaskPrefix + suggestion, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestConfirmGeneratedTaskAppendsToExhaustedSource(t *testing.T) {
	// Issue #157: when the agent already has a declared task source (whose
	// checklist ran dry and triggered generation), confirming appends the
	// generated task to THAT file — it must not write a second per-agent
	// tasks.md and register a duplicate [[task_sources]] entry, which makes
	// `hap task <agent>` ambiguous.
	app, st, fake, name, path := declaredSourceApp(t, "w1:p1", "- [x] 1. old task\n")
	ctx := context.Background()
	taskText := "Investigate the flaky auth test and add a retry guard"
	id := generatedEscalation(t, st, "w1:p1", taskText)

	if err := app.Confirm(ctx, id, true); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Appended to the declared file, delivered task flipped in-progress, the
	// existing completed item untouched.
	want := "- [x] 1. old task\n- [-] " + taskText + "\n"
	if string(body) != want {
		t.Errorf("declared file = %q, want %q", body, want)
	}

	// No per-agent bootstrap file, and still exactly ONE task source with the
	// original path.
	if _, err := os.Stat(filepath.Join(app.StateDir, "tasks")); !os.IsNotExist(err) {
		t.Errorf("no <state>/tasks bootstrap dir may be created, stat err = %v", err)
	}
	cfg, _ := config.Load(app.ConfigPath)
	if len(cfg.TaskSources) != 1 || cfg.TaskSources[0].Path != path {
		t.Fatalf("want exactly 1 task source at %q, got %+v", path, cfg.TaskSources)
	}

	// The prompt points at the DECLARED file, and the correction learns the
	// declared-task action.
	wantPrompt := domain.DeclaredTask{Task: taskText, Path: path, AgentName: name}.Prompt()
	if len(fake.inputs) != 1 || fake.inputs[0] != wantPrompt {
		t.Errorf("delivered %v, want the declared-source prompt %q", fake.inputs, wantPrompt)
	}
	corr, _ := st.UnprocessedCorrections(ctx)
	if len(corr) != 1 || corr[0].CorrectedAction != domain.ActionNextDeclaredTask {
		t.Errorf("confirm should record a declared-task correction: %+v", corr)
	}
	audit, _ := st.GetAudit(ctx, id)
	if audit.Status != "resolved" {
		t.Errorf("escalation status = %q, want resolved", audit.Status)
	}
}

func TestConfirmGeneratedMultipleTasksAppendToExhaustedSource(t *testing.T) {
	// A multi-task suggestion appends every task to the declared file: the
	// delivered first task in-progress, the rest pending for the normal
	// declared-task flow. Only the first is sent.
	app, st, fake, name, path := declaredSourceApp(t, "w1:p1", "- [x] 1. old task\n")
	ctx := context.Background()
	suggestion := "- [ ] Investigate the flaky auth test\n- [ ] Add a retry guard\n- [ ] Backfill unit tests"
	id := generatedEscalation(t, st, "w1:p1", suggestion)

	if err := app.Confirm(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(path)
	want := "- [x] 1. old task\n- [-] Investigate the flaky auth test\n- [ ] Add a retry guard\n- [ ] Backfill unit tests\n"
	if string(body) != want {
		t.Errorf("declared file = %q, want %q", body, want)
	}
	wantPrompt := domain.DeclaredTask{Task: "Investigate the flaky auth test", Path: path, AgentName: name}.Prompt()
	if len(fake.inputs) != 1 || fake.inputs[0] != wantPrompt {
		t.Errorf("delivered %v, want only the first task as %q", fake.inputs, wantPrompt)
	}
	// The queue drives on later idles from the declared file.
	if next := domain.NextDeclaredTask(string(body)); next != "Add a retry guard" {
		t.Errorf("next declared task = %q, want the first appended pending item", next)
	}
}

func TestConfirmGeneratedTaskAppendWithoutSend(t *testing.T) {
	// send=false appends every task pending ("[ ]") and delivers nothing: the
	// daemon's declared flow hands them out on later idles. No in-progress
	// marker may be left behind (an undelivered "[-]" is never re-sent).
	app, st, fake, _, path := declaredSourceApp(t, "w2:p2", "- [x] 1. old task\n")
	ctx := context.Background()
	id := generatedEscalation(t, st, "w2:p2", "Write missing tests")

	if err := app.Confirm(ctx, id, false); err != nil {
		t.Fatal(err)
	}
	if len(fake.inputs) != 0 {
		t.Errorf("send=false must deliver nothing, got %v", fake.inputs)
	}
	body, _ := os.ReadFile(path)
	want := "- [x] 1. old task\n- [ ] Write missing tests\n"
	if string(body) != want {
		t.Errorf("declared file = %q, want %q", body, want)
	}
	cfg, _ := config.Load(app.ConfigPath)
	if len(cfg.TaskSources) != 1 {
		t.Errorf("want exactly 1 task source, got %d", len(cfg.TaskSources))
	}
	audit, _ := st.GetAudit(ctx, id)
	if audit.Status != "resolved" {
		t.Errorf("escalation status = %q, want resolved", audit.Status)
	}
}

func TestConfirmGeneratedTaskAppendIsIdempotent(t *testing.T) {
	// The atomic claim gates the (non-idempotent) append: a double-submit must
	// not append the task twice or send twice.
	app, st, fake, _, path := declaredSourceApp(t, "w3:p3", "- [x] 1. old task\n")
	ctx := context.Background()
	id := generatedEscalation(t, st, "w3:p3", "Do the thing")

	if err := app.Confirm(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	if err := app.Confirm(ctx, id, true); err == nil {
		t.Error("second confirm on a resolved escalation must fail")
	}
	body, _ := os.ReadFile(path)
	if got := strings.Count(string(body), "Do the thing"); got != 1 {
		t.Errorf("task appended %d times, want exactly once: %q", got, body)
	}
	if len(fake.inputs) != 1 {
		t.Errorf("task must be sent exactly once, got %d sends", len(fake.inputs))
	}
}

func TestConfirmGeneratedTaskAppendRespectsMaxTasks(t *testing.T) {
	// The confirm-time append honors the source's max_tasks cap — the same
	// limit the daemon's generation gate and manual `task add` enforce.
	t.Run("full list refuses and stays pending", func(t *testing.T) {
		app, st, fake, _, path := declaredSourceApp(t, "w1:p1", "- [x] 1. a\n- [x] 2. b\n")
		ctx := context.Background()
		if err := app.UpdateConfig(ctx, func(cfg *config.Config) error {
			cfg.TaskSources[0].MaxTasks = 2
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		id := generatedEscalation(t, st, "w1:p1", "One more thing")

		if err := app.Confirm(ctx, id, true); err == nil ||
			!strings.Contains(err.Error(), "maximum number of tasks") {
			t.Fatalf("confirm on a full list must refuse with the cap error, got %v", err)
		}
		body, _ := os.ReadFile(path)
		if string(body) != "- [x] 1. a\n- [x] 2. b\n" {
			t.Errorf("full list must stay unchanged, got %q", body)
		}
		if len(fake.inputs) != 0 {
			t.Errorf("nothing may be sent on a refused confirm, got %v", fake.inputs)
		}
		audit, _ := st.GetAudit(ctx, id)
		if audit.Status != "escalated" {
			t.Errorf("escalation must stay pending so the operator can prune and retry, got %q", audit.Status)
		}
		// The refusal is retryable: raise the cap and the SAME escalation
		// confirms cleanly, appending exactly one copy.
		if err := app.UpdateConfig(ctx, func(cfg *config.Config) error {
			cfg.TaskSources[0].MaxTasks = 10
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := app.Confirm(ctx, id, true); err != nil {
			t.Fatalf("re-confirm after raising the cap must succeed, got %v", err)
		}
		body, _ = os.ReadFile(path)
		if got := strings.Count(string(body), "One more thing"); got != 1 {
			t.Errorf("retried confirm appended %d copies, want exactly 1: %q", got, body)
		}
		if !strings.Contains(string(body), "- [-] One more thing") {
			t.Errorf("retried confirm must leave the delivered task reserved, got %q", body)
		}
		if len(fake.inputs) != 1 {
			t.Errorf("the retried confirm must send exactly once, got %d sends", len(fake.inputs))
		}
	})
	t.Run("partial overflow refuses without truncating", func(t *testing.T) {
		// 1 existing + 3 new = 4 exceeds the cap of 3. Rather than silently
		// appending only the 2 that fit and dropping "third", the confirm
		// refuses the whole set so the operator sees every suggested task and
		// prunes the list — hiding work would be worse than refusing.
		app, st, fake, _, path := declaredSourceApp(t, "w1:p1", "- [x] 1. a\n")
		ctx := context.Background()
		if err := app.UpdateConfig(ctx, func(cfg *config.Config) error {
			cfg.TaskSources[0].MaxTasks = 3
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		id := generatedEscalation(t, st, "w1:p1", "- [ ] first\n- [ ] second\n- [ ] third")

		err := app.Confirm(ctx, id, true)
		if err == nil || !strings.Contains(err.Error(), "maximum number of tasks") {
			t.Fatalf("a would-be-over-cap append must refuse with the cap error, got %v", err)
		}
		body, _ := os.ReadFile(path)
		if string(body) != "- [x] 1. a\n" {
			t.Errorf("nothing may be appended on a refused confirm, got %q", body)
		}
		if len(fake.inputs) != 0 {
			t.Errorf("nothing may be sent on a refused confirm, got %v", fake.inputs)
		}
		audit, _ := st.GetAudit(ctx, id)
		if audit.Status != "escalated" {
			t.Errorf("escalation must stay pending so the operator can prune and retry, got %q", audit.Status)
		}
	})
	t.Run("append that fits the cap exactly succeeds", func(t *testing.T) {
		// 1 existing + 2 new = 3 == cap: the boundary is inclusive, so this must
		// go through (the refusal is for exceeding, not reaching, the cap).
		app, st, fake, _, path := declaredSourceApp(t, "w1:p1", "- [x] 1. a\n")
		ctx := context.Background()
		if err := app.UpdateConfig(ctx, func(cfg *config.Config) error {
			cfg.TaskSources[0].MaxTasks = 3
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		id := generatedEscalation(t, st, "w1:p1", "- [ ] first\n- [ ] second")

		if err := app.Confirm(ctx, id, true); err != nil {
			t.Fatalf("an append that exactly fills the cap must succeed, got %v", err)
		}
		body, _ := os.ReadFile(path)
		want := "- [x] 1. a\n- [-] first\n- [ ] second\n"
		if string(body) != want {
			t.Errorf("declared file = %q, want both tasks appended %q", body, want)
		}
		if len(fake.inputs) != 1 {
			t.Errorf("the first task must be sent, got %v", fake.inputs)
		}
	})
}

func TestConfirmGeneratedTaskBootstrapRespectsMaxTasks(t *testing.T) {
	// The bootstrap path (no declared source yet) also honors the default
	// max_tasks cap: a file already holding DefaultMaxTasks items refuses one
	// more generated task instead of growing an unbounded list. Regression for
	// the gap where only the append + `task add` paths enforced the cap.
	app, st := testApp(t)
	fake := &fakeHerdr{}
	app.Herdr = fake
	stateDir := t.TempDir()
	app.StateDir = stateDir
	ctx := context.Background()

	name, _ := st.EnsureAgentName(ctx, "w1:p1")
	// Pre-seed the bootstrap file AT the cap without registering a source, so
	// the confirm takes the bootstrap branch (not the declared-source append).
	path := filepath.Join(stateDir, "tasks", name+".md")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("# Tasks for " + name + "\n\n")
	for i := 1; i <= config.DefaultMaxTasks; i++ {
		fmt.Fprintf(&b, "- [ ] %d. task %d\n", i, i)
	}
	seeded := b.String()
	if err := os.WriteFile(path, []byte(seeded), 0o600); err != nil {
		t.Fatal(err)
	}

	id, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w1:p1", SituationType: domain.SituationIdle, Trigger: "t",
		Action: "escalated", Status: "escalated",
		Suggestion: domain.SuggestTaskPrefix + "one more task", CreatedAt: time.Now(),
	})

	err := app.Confirm(ctx, id, true)
	if err == nil || !strings.Contains(err.Error(), "maximum number of tasks") {
		t.Fatalf("a bootstrap confirm over the default cap must refuse, got %v", err)
	}
	body, _ := os.ReadFile(path)
	if string(body) != seeded {
		t.Errorf("a refused bootstrap confirm must not change the file, got %q", body)
	}
	if len(fake.inputs) != 0 {
		t.Errorf("nothing may be sent on a refused confirm, got %v", fake.inputs)
	}
	// The refusal happens before source registration, so the operator can prune
	// the file and retry the same escalation.
	cfg, err := config.Load(app.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.TaskSources) != 0 {
		t.Errorf("a refused confirm must not register a source, got %+v", cfg.TaskSources)
	}
	audit, _ := st.GetAudit(ctx, id)
	if audit.Status != "escalated" {
		t.Errorf("escalation must stay pending so the operator can prune and retry, got %q", audit.Status)
	}
}

func TestConfirmGeneratedTaskBootstrapOverCapNoGrowthNotRefused(t *testing.T) {
	// An already-over-cap bootstrap file (a pre-fix write or a hand edit) with
	// NON-canonical numbering re-confirmed with only already-present tasks adds
	// nothing, so it must NOT be refused — the cap gate keys on genuinely-new
	// tasks, not on a text/numbering match. Regression: keying the exemption on
	// sameChecklistTexts stranded the escalation of a reordered over-cap file.
	app, st := testApp(t)
	fake := &fakeHerdr{}
	app.Herdr = fake
	stateDir := t.TempDir()
	app.StateDir = stateDir
	ctx := context.Background()

	name, _ := st.EnsureAgentName(ctx, "w1:p1")
	path := filepath.Join(stateDir, "tasks", name+".md")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	// One MORE than the cap, numbered in REVERSE so the file's rendered text
	// differs from RenderGeneratedTaskList's canonical 1..N — this makes
	// sameChecklistTexts return false and exercises the cap gate directly.
	n := config.DefaultMaxTasks + 1
	var b strings.Builder
	b.WriteString("# Tasks for " + name + "\n\n")
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "- [ ] %d. task %d\n", n+1-i, i)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	// The suggestion is an item ALREADY present (identity "task 1"), so the
	// merge adds no new task. send=false keeps the assertion on the refusal
	// gate, not on delivery.
	id, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w1:p1", SituationType: domain.SituationIdle, Trigger: "t",
		Action: "escalated", Status: "escalated",
		Suggestion: domain.SuggestTaskPrefix + "task 1", CreatedAt: time.Now(),
	})

	if err := app.Confirm(ctx, id, false); err != nil {
		t.Fatalf("a no-growth re-confirm of an over-cap file must not be refused, got %v", err)
	}
	// The list neither grew nor shrank — still n items, just renumbered.
	body, _ := os.ReadFile(path)
	if got := len(domain.ParseChecklist(string(body))); got != n {
		t.Errorf("over-cap file must be preserved at %d items, got %d: %q", n, got, body)
	}
	// The escalation resolved (not stranded).
	audit, _ := st.GetAudit(ctx, id)
	if audit.Status == "escalated" {
		t.Errorf("a no-growth confirm must resolve the escalation, got %q", audit.Status)
	}
}

// sendHookHerdr wraps fakeHerdr to observe on-disk state at the exact moment
// Send runs — the only way to assert reserve-BEFORE-send ordering, since the
// final file state after a rollback is identical to never having reserved.
type sendHookHerdr struct {
	*fakeHerdr
	onSend func()
}

func (h *sendHookHerdr) Send(ctx context.Context, paneID, input string) error {
	if h.onSend != nil {
		h.onSend()
	}
	return h.fakeHerdr.Send(ctx, paneID, input)
}

func TestConfirmGeneratedTaskAppendReservesAndRollsBackOnSendFailure(t *testing.T) {
	// The delivery mirrors SendTaskToAgent: the first task is reserved [-]
	// before the send (so the daemon's idle flow can never hand it out
	// mid-send), and a failed send releases it back to [ ] so the declared
	// flow delivers it on a later idle — never a stranded [-] nobody will
	// send.
	app, st, fake, _, path := declaredSourceApp(t, "w5:p5", "- [x] 1. old task\n")
	fake.sendErr = errors.New("induced send failure")
	reservedMidSend := false
	app.Herdr = &sendHookHerdr{fakeHerdr: fake, onSend: func() {
		body, _ := os.ReadFile(path)
		reservedMidSend = strings.Contains(string(body), "- [-] Deliver me later")
	}}
	ctx := context.Background()
	id := generatedEscalation(t, st, "w5:p5", "Deliver me later")

	err := app.Confirm(ctx, id, true)
	if err == nil || !strings.Contains(err.Error(), "sending the task to the agent failed") {
		t.Fatalf("confirm with a failing send must surface the send error, got %v", err)
	}
	if !reservedMidSend {
		t.Error("the task must already be reserved [-] while Send is in flight")
	}
	body, _ := os.ReadFile(path)
	want := "- [x] 1. old task\n- [ ] Deliver me later\n"
	if string(body) != want {
		t.Errorf("declared file = %q, want the task released to pending %q", body, want)
	}
	// The claim gates the send, so the escalation is consumed even though the
	// send failed — the appended task is recovered via the declared flow (or
	// `hap task send`), not by re-confirming.
	audit, _ := st.GetAudit(ctx, id)
	if audit.Status != "resolved" {
		t.Errorf("escalation status = %q, want resolved (claim precedes the send)", audit.Status)
	}
}

func TestConfirmGeneratedTaskSendRefusedWhenFirstTaskNotPending(t *testing.T) {
	// A suggestion whose first task already sits [x]/[-] in the declared file
	// cannot be delivered: the refusal must land PRE-claim (escalation stays
	// pending, actionable) instead of surfacing from reserveTask after the
	// claim consumed it.
	app, st, fake, _, path := declaredSourceApp(t, "w6:p6", "- [x] Deliver me later\n")
	ctx := context.Background()
	id := generatedEscalation(t, st, "w6:p6", "Deliver me later")

	err := app.Confirm(ctx, id, true)
	if err == nil || !strings.Contains(err.Error(), "already [x]") {
		t.Fatalf("confirm with an already-done first task must refuse with the mark, got %v", err)
	}
	if len(fake.inputs) != 0 {
		t.Errorf("nothing may be sent, got %v", fake.inputs)
	}
	audit, _ := st.GetAudit(ctx, id)
	if audit.Status != "escalated" {
		t.Errorf("escalation must stay pending after the pre-claim refusal, got %q", audit.Status)
	}
	body, _ := os.ReadFile(path)
	if string(body) != "- [x] Deliver me later\n" {
		t.Errorf("declared file must stay unchanged, got %q", body)
	}
}

func TestConfirmGeneratedTaskAppendDeduplicatesRepeatedSuggestion(t *testing.T) {
	// A suggestion repeating the same task text appends it once — a
	// repetitive LLM output must not stack duplicate checklist items or burn
	// cap room on copies.
	app, st, fake, _, path := declaredSourceApp(t, "w7:p7", "- [x] 1. old task\n")
	ctx := context.Background()
	id := generatedEscalation(t, st, "w7:p7", "- [ ] Same thing\n- [ ] Other thing\n- [ ] Same thing")

	if err := app.Confirm(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(path)
	want := "- [x] 1. old task\n- [-] Same thing\n- [ ] Other thing\n"
	if string(body) != want {
		t.Errorf("declared file = %q, want the repeated task appended once: %q", body, want)
	}
	if len(fake.inputs) != 1 {
		t.Errorf("only the first task may be sent, got %v", fake.inputs)
	}
}

func TestConfirmGeneratedTaskUsesSourceTemplate(t *testing.T) {
	// The append path renders the outbound prompt through the SOURCE's own
	// next_task_template, like any declared-task send — not the built-in
	// default the bootstrap path uses.
	app, st := testApp(t)
	fake := &fakeHerdr{}
	app.Herdr = fake
	app.StateDir = t.TempDir()
	ctx := context.Background()
	name, _ := st.EnsureAgentName(ctx, "w1:p1")
	path := filepath.Join(t.TempDir(), "declared.md")
	if err := os.WriteFile(path, []byte("- [x] 1. old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tpl := "DO {next_task_content} FROM {task_list_path} AS {agent_name}"
	if err := app.AddTaskSource(ctx, name, "", path, tpl); err != nil {
		t.Fatal(err)
	}
	id := generatedEscalation(t, st, "w1:p1", "Ship it")

	if err := app.Confirm(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	want := "DO Ship it FROM " + path + " AS " + name
	if len(fake.inputs) != 1 || fake.inputs[0] != want {
		t.Errorf("delivered %v, want the source-template prompt %q", fake.inputs, want)
	}
}

func TestConfirmGeneratedTaskAppendCreatesMissingDeclaredFile(t *testing.T) {
	// A declared source whose file does not exist yet still receives the
	// tasks at ITS path — never a second bootstrap source.
	app, st := testApp(t)
	fake := &fakeHerdr{}
	app.Herdr = fake
	app.StateDir = t.TempDir()
	ctx := context.Background()
	name, _ := st.EnsureAgentName(ctx, "w1:p1")
	path := filepath.Join(t.TempDir(), "sub", "declared.md")
	if err := app.AddTaskSource(ctx, name, "", path, ""); err != nil {
		t.Fatal(err)
	}
	id := generatedEscalation(t, st, "w1:p1", "Bootstrap the declared file")

	if err := app.Confirm(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("declared file not created: %v", err)
	}
	if !strings.Contains(string(body), "- [-] Bootstrap the declared file") {
		t.Errorf("declared file = %q, want the in-progress appended task", body)
	}
	cfg, _ := config.Load(app.ConfigPath)
	if len(cfg.TaskSources) != 1 || cfg.TaskSources[0].Path != path {
		t.Fatalf("want exactly 1 task source at %q, got %+v", path, cfg.TaskSources)
	}
}

// locatorHerdr wraps fakeHerdr with a LocatorPort so workspace-scoped
// selectors can resolve display names at confirm time.
type locatorHerdr struct {
	*fakeHerdr
	workspaces []domain.WorkspaceInfo
}

func (l *locatorHerdr) ListWorkspaces(context.Context) ([]domain.WorkspaceInfo, error) {
	return l.workspaces, nil
}

func (l *locatorHerdr) ListTabs(context.Context) ([]domain.TabInfo, error) { return nil, nil }

func TestConfirmGeneratedTaskAppendMatchesDaemonSelectors(t *testing.T) {
	// The confirm-time source resolution must use the daemon's selector
	// semantics — agent id, agent type, and workspace name/id scoping — not
	// just the short name, or an id-/type-selected declared source would be
	// bypassed and bootstrapped into a duplicate.
	t.Run("agent id selector", func(t *testing.T) {
		app, st := testApp(t)
		fake := &fakeHerdr{}
		app.Herdr = fake
		app.StateDir = t.TempDir()
		ctx := context.Background()
		path := filepath.Join(t.TempDir(), "declared.md")
		os.WriteFile(path, []byte("- [x] 1. old\n"), 0o600)
		if err := app.AddTaskSource(ctx, "w1:p1", "", path, ""); err != nil {
			t.Fatal(err)
		}
		id := generatedEscalation(t, st, "w1:p1", "By id")
		if err := app.Confirm(ctx, id, false); err != nil {
			t.Fatal(err)
		}
		body, _ := os.ReadFile(path)
		if !strings.Contains(string(body), "- [ ] By id") {
			t.Errorf("declared file = %q, want the appended task", body)
		}
	})
	t.Run("agent type selector", func(t *testing.T) {
		app, st := testApp(t)
		fake := &fakeHerdr{}
		app.Herdr = fake
		app.StateDir = t.TempDir()
		ctx := context.Background()
		path := filepath.Join(t.TempDir(), "declared.md")
		os.WriteFile(path, []byte("- [x] 1. old\n"), 0o600)
		if err := app.AddTaskSource(ctx, "claude", "", path, ""); err != nil {
			t.Fatal(err)
		}
		id, err := st.AppendAudit(ctx, domain.AuditRecord{
			AgentID: "w1:p1", AgentType: "claude", SituationType: domain.SituationIdle,
			Trigger: "t", Action: "escalated", Status: "escalated",
			Suggestion: domain.SuggestTaskPrefix + "By type", CreatedAt: time.Now(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := app.Confirm(ctx, id, false); err != nil {
			t.Fatal(err)
		}
		body, _ := os.ReadFile(path)
		if !strings.Contains(string(body), "- [ ] By type") {
			t.Errorf("declared file = %q, want the appended task", body)
		}
	})
	t.Run("workspace name selector via locator", func(t *testing.T) {
		app, st := testApp(t)
		fake := &locatorHerdr{
			fakeHerdr:  &fakeHerdr{agents: []domain.AgentTransition{{AgentID: "w1:p1", Status: "idle", WorkspaceID: "ws-1"}}},
			workspaces: []domain.WorkspaceInfo{{ID: "ws-1", Label: "codex-main"}},
		}
		app.Herdr = fake
		app.StateDir = t.TempDir()
		ctx := context.Background()
		name, _ := st.EnsureAgentName(ctx, "w1:p1")
		path := filepath.Join(t.TempDir(), "declared.md")
		os.WriteFile(path, []byte("- [x] 1. old\n"), 0o600)
		if err := app.AddTaskSource(ctx, name, "codex-*", path, ""); err != nil {
			t.Fatal(err)
		}
		id := generatedEscalation(t, st, "w1:p1", "By workspace name")
		if err := app.Confirm(ctx, id, false); err != nil {
			t.Fatal(err)
		}
		body, _ := os.ReadFile(path)
		if !strings.Contains(string(body), "- [ ] By workspace name") {
			t.Errorf("declared file = %q, want the appended task", body)
		}
		cfg, _ := config.Load(app.ConfigPath)
		if len(cfg.TaskSources) != 1 {
			t.Errorf("want exactly 1 task source, got %+v", cfg.TaskSources)
		}
	})
	t.Run("workspace raw id fallback without locator", func(t *testing.T) {
		app, st := testApp(t)
		fake := &fakeHerdr{agents: []domain.AgentTransition{{AgentID: "w1:p1", Status: "idle", WorkspaceID: "ws-1"}}}
		app.Herdr = fake
		app.StateDir = t.TempDir()
		ctx := context.Background()
		name, _ := st.EnsureAgentName(ctx, "w1:p1")
		path := filepath.Join(t.TempDir(), "declared.md")
		os.WriteFile(path, []byte("- [x] 1. old\n"), 0o600)
		if err := app.AddTaskSource(ctx, name, "ws-1", path, ""); err != nil {
			t.Fatal(err)
		}
		id := generatedEscalation(t, st, "w1:p1", "By raw workspace id")
		if err := app.Confirm(ctx, id, false); err != nil {
			t.Fatal(err)
		}
		body, _ := os.ReadFile(path)
		if !strings.Contains(string(body), "- [ ] By raw workspace id") {
			t.Errorf("declared file = %q, want the appended task", body)
		}
	})
}

func TestConfirmGeneratedTaskPrefersSourceWithPendingWork(t *testing.T) {
	// With several matching sources, the append lands on the one the daemon
	// would reason about: a source with a pending "[ ]" item beats a fully
	// completed one, regardless of config order.
	app, st := testApp(t)
	fake := &fakeHerdr{}
	app.Herdr = fake
	app.StateDir = t.TempDir()
	ctx := context.Background()
	name, _ := st.EnsureAgentName(ctx, "w1:p1")
	dir := t.TempDir()
	donePath := filepath.Join(dir, "done.md")
	pendingPath := filepath.Join(dir, "pending.md")
	os.WriteFile(donePath, []byte("- [x] 1. finished\n"), 0o600)
	os.WriteFile(pendingPath, []byte("- [ ] 1. queued\n"), 0o600)
	// The completed source is registered FIRST, so config order alone would
	// pick the wrong file.
	if err := app.AddTaskSource(ctx, name, "", donePath, ""); err != nil {
		t.Fatal(err)
	}
	if err := app.AddTaskSource(ctx, name, "", pendingPath, ""); err != nil {
		t.Fatal(err)
	}
	id := generatedEscalation(t, st, "w1:p1", "Go to the live list")

	if err := app.Confirm(ctx, id, false); err != nil {
		t.Fatal(err)
	}
	pendingBody, _ := os.ReadFile(pendingPath)
	if !strings.Contains(string(pendingBody), "- [ ] Go to the live list") {
		t.Errorf("pending-work source = %q, want the appended task there", pendingBody)
	}
	doneBody, _ := os.ReadFile(donePath)
	if strings.Contains(string(doneBody), "Go to the live list") {
		t.Errorf("completed source must stay untouched, got %q", doneBody)
	}
}

func TestConfirmGeneratedTaskRefusesDuplicateAgentSource(t *testing.T) {
	// Defense-in-depth: a source registered under this agent's name but scoped
	// to a workspace the confirm cannot match falls through to the bootstrap
	// path — which must REFUSE to register a second source for the same agent
	// selector (that duplicate is exactly the `hap task` ambiguity of #157).
	app, st := testApp(t)
	fake := &fakeHerdr{}
	app.Herdr = fake
	app.StateDir = t.TempDir()
	ctx := context.Background()
	name, _ := st.EnsureAgentName(ctx, "w1:p1")
	path := filepath.Join(t.TempDir(), "declared.md")
	if err := os.WriteFile(path, []byte("- [x] 1. old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := app.AddTaskSource(ctx, name, "other-workspace", path, ""); err != nil {
		t.Fatal(err)
	}
	id := generatedEscalation(t, st, "w1:p1", "Do the thing")

	if err := app.Confirm(ctx, id, true); err == nil ||
		!strings.Contains(err.Error(), "already has a task source") {
		t.Fatalf("confirm must refuse to register a duplicate agent source, got %v", err)
	}
	cfg, _ := config.Load(app.ConfigPath)
	if len(cfg.TaskSources) != 1 {
		t.Errorf("config must be unchanged, got %+v", cfg.TaskSources)
	}
	if len(fake.inputs) != 0 {
		t.Errorf("nothing may be sent, got %v", fake.inputs)
	}
	audit, _ := st.GetAudit(ctx, id)
	if audit.Status != "escalated" {
		t.Errorf("escalation must stay pending, got %q", audit.Status)
	}
}

func TestConfirmDeliversMenuDigitNotLabel(t *testing.T) {
	// Regression: an LLM/learned approval carries the option LABEL ("Yes"),
	// but Claude's numbered menu only accepts the digit. Confirm must
	// re-read the live pane and deliver "1", not "Yes".
	app, st := testApp(t)
	fake := &fakeHerdr{pane: "Do you want to proceed?\n❯ 1. Yes\n  2. No, and tell the agent\n"}
	app.Herdr = fake
	ctx := context.Background()

	id, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w1:p1", SituationType: domain.SituationApproval, Trigger: "t",
		Action: "escalated", Status: "escalated",
		Suggestion: "LLM suggested: Yes", CreatedAt: time.Now(),
	})
	if err := app.Confirm(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	if len(fake.inputs) != 1 || fake.inputs[0] != "1" {
		t.Errorf("delivered %v, want the menu digit [\"1\"]", fake.inputs)
	}
}

func TestConfirmFreeTextPromptDeliveredLiterally(t *testing.T) {
	// A pane with no numbered menu (free-text reply) must receive the
	// literal action, not be mangled by menu mapping.
	app, st := testApp(t)
	fake := &fakeHerdr{pane: "Enter a commit message:\n> "}
	app.Herdr = fake
	ctx := context.Background()

	id, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w1:p1", SituationType: domain.SituationApproval, Trigger: "t",
		Action: "escalated", Status: "escalated",
		Suggestion: "respond: fix: the bug", CreatedAt: time.Now(),
	})
	if err := app.Confirm(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	if len(fake.inputs) != 1 || fake.inputs[0] != "fix: the bug" {
		t.Errorf("delivered %v, want the literal reply", fake.inputs)
	}
}

func TestConfirmMenuUnreadableFallsBackToLabel(t *testing.T) {
	// If the pane can't be re-read, the confirm still delivers (the literal
	// label) rather than dropping the send.
	app, st := testApp(t)
	fake := &fakeHerdr{readErr: errAny}
	app.Herdr = fake
	ctx := context.Background()

	id, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w1:p1", SituationType: domain.SituationApproval, Trigger: "t",
		Action: "escalated", Status: "escalated",
		Suggestion: "LLM suggested: Yes", CreatedAt: time.Now(),
	})
	if err := app.Confirm(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	if len(fake.inputs) != 1 || fake.inputs[0] != "Yes" {
		t.Errorf("delivered %v, want the literal label fallback", fake.inputs)
	}
}

func TestDismissEscalation(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	id, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "a1", SituationType: domain.SituationApproval, Trigger: "t",
		Action: "escalated", Status: "escalated", Suggestion: "respond: y", CreatedAt: time.Now(),
	})

	if err := app.Dismiss(ctx, id); err != nil {
		t.Fatal(err)
	}
	// Gone from the pending queue, kept in the audit log as dismissed.
	esc, _ := app.Escalations(ctx)
	if len(esc) != 0 {
		t.Errorf("dismissed escalation still pending: %+v", esc)
	}
	rec, _ := st.GetAudit(ctx, id)
	if rec == nil || rec.Status != "dismissed" {
		t.Fatalf("audit row must be kept as dismissed, got %+v", rec)
	}
	// A dismiss must never become a learning event.
	if corr, _ := st.UnprocessedCorrections(ctx); len(corr) != 0 {
		t.Errorf("dismiss must not record a correction: %+v", corr)
	}
}

func TestDismissRejectsNonPending(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	if err := app.Dismiss(ctx, 999); err == nil {
		t.Error("dismissing a missing audit record must fail")
	}
	id, _ := st.AppendAudit(ctx, domain.AuditRecord{
		SituationType: domain.SituationChoice, Trigger: "t",
		Action: "2", Status: "auto", CreatedAt: time.Now(),
	})
	if err := app.Dismiss(ctx, id); err == nil {
		t.Error("dismissing a non-escalated record must fail")
	}
	if rec, _ := st.GetAudit(ctx, id); rec == nil || rec.Status != "auto" {
		t.Errorf("rejected dismiss must not change the row, got %+v", rec)
	}
}

func TestRetryLLMQueuesForFailedConsult(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	id, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "a1", SituationType: domain.SituationApproval, Trigger: "agent-status: blocked",
		Action: "escalated", Rationale: "[llm_timeout] llm timeout after 2m0s without submit_decision",
		Status: "escalated", CreatedAt: time.Now(),
	})

	if err := app.RetryLLM(ctx, id); err != nil {
		t.Fatal(err)
	}
	q, err := st.UnprocessedLLMRetries(ctx)
	if err != nil || len(q) != 1 || q[0].AuditID != id {
		t.Fatalf("retry should be queued for audit %d, got %+v %v", id, q, err)
	}
	// The escalation is unchanged — a fresh outcome writes its own audit row.
	if rec, _ := st.GetAudit(ctx, id); rec == nil || rec.Status != "escalated" {
		t.Errorf("retry must not change the escalation status, got %+v", rec)
	}
}

func TestRetryLLMRejectsNonRetryable(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()

	if err := app.RetryLLM(ctx, 999); err == nil {
		t.Error("retrying a missing audit record must fail")
	}

	// A gated-but-answered escalation (shadow_mode) is not an LLM failure:
	// re-invoking would hit the same gate, so it is not retryable.
	shadowID, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "a1", SituationType: domain.SituationApproval, Trigger: "t",
		Action: "escalated", Rationale: "[shadow_mode]", Status: "escalated", CreatedAt: time.Now(),
	})
	if err := app.RetryLLM(ctx, shadowID); err == nil {
		t.Error("retrying a non-LLM-failure escalation must fail")
	}
	if q, _ := st.UnprocessedLLMRetries(ctx); len(q) != 0 {
		t.Errorf("rejected retry must not queue anything, got %+v", q)
	}
}

func TestHasPendingLLMConsult(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()

	if pending, err := app.HasPendingLLMConsult(ctx, "a1"); err != nil || pending {
		t.Fatalf("no consult staged: got %v %v, want false", pending, err)
	}
	if _, err := st.StageLLMRequest(ctx, domain.LLMRequest{
		RequestID: "req-a1-1", Signature: "sig", SituationType: domain.SituationApproval,
		AgentType: "claude", AgentID: "a1", ContextJSON: "{}", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if pending, err := app.HasPendingLLMConsult(ctx, "a1"); err != nil || !pending {
		t.Fatalf("consult in flight: got %v %v, want true", pending, err)
	}
}

func TestPruneEscalations(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	oldID, _ := st.AppendAudit(ctx, domain.AuditRecord{
		SituationType: domain.SituationApproval, Trigger: "old",
		Action: "escalated", Status: "escalated", CreatedAt: time.Now().Add(-7 * time.Hour),
	})
	freshID, _ := st.AppendAudit(ctx, domain.AuditRecord{
		SituationType: domain.SituationApproval, Trigger: "fresh",
		Action: "escalated", Status: "escalated", CreatedAt: time.Now().Add(-time.Minute),
	})

	n, err := app.PruneEscalations(ctx, 6*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("pruned %d escalation(s), want 1", n)
	}
	if rec, _ := st.GetAudit(ctx, oldID); rec == nil || rec.Status != "dismissed" {
		t.Errorf("old escalation must be dismissed, got %+v", rec)
	}
	if rec, _ := st.GetAudit(ctx, freshID); rec == nil || rec.Status != "escalated" {
		t.Errorf("fresh escalation must stay pending, got %+v", rec)
	}
	if _, err := app.PruneEscalations(ctx, 0); err == nil {
		t.Error("a non-positive prune age must be rejected")
	}
}

func TestResolveUnknownAuditFails(t *testing.T) {
	app, _ := testApp(t)
	if err := app.Resolve(context.Background(), 999, "x", false); err == nil {
		t.Error("resolving a missing audit record must fail")
	}
}

func TestSetThresholdPersists(t *testing.T) {
	app, _ := testApp(t)
	ctx := context.Background()
	if err := app.SetThreshold(ctx, "approval", 0.93); err != nil {
		t.Fatal(err)
	}
	cfg, err := app.Config()
	if err != nil || cfg.ConfidenceThresholds.Approval != 0.93 {
		t.Fatalf("threshold not persisted: %+v %v", cfg.ConfidenceThresholds, err)
	}
	if err := app.SetThreshold(ctx, "approval", 1.5); err == nil {
		t.Error("out-of-range threshold must be rejected")
	}
	if err := app.SetThreshold(ctx, "bogus", 0.5); err == nil {
		t.Error("unknown situation must be rejected")
	}
	if err := app.SetThreshold(ctx, "minimum", 0.55); err != nil {
		t.Fatal(err)
	}
	cfg, err = app.Config()
	if err != nil || cfg.ConfidenceThresholds.Minimum != 0.55 {
		t.Fatalf("minimum agreement not persisted: %+v %v", cfg.ConfidenceThresholds, err)
	}
}

func TestAddNeverAutoPatternValidates(t *testing.T) {
	app, _ := testApp(t)
	ctx := context.Background()
	if err := app.AddNeverAutoPattern(ctx, `(?i)restart\s+prod`); err != nil {
		t.Fatal(err)
	}
	cfg, _ := app.Config()
	if len(cfg.Safety.NeverAutoPatterns) != 1 {
		t.Error("pattern not persisted")
	}
	if err := app.AddNeverAutoPattern(ctx, "([broken"); err == nil {
		t.Error("invalid regex must be rejected")
	}
}

func TestSetFieldValidatesAndPersists(t *testing.T) {
	app, _ := testApp(t)
	ctx := context.Background()

	cases := []struct {
		key, value string
		wantErr    bool
	}{
		{"confidence_thresholds.minimum", "0.55", false},
		{"confidence_thresholds.approval", "0.92", false},
		{"confidence_thresholds.approval", "1.5", true},
		{"confidence_thresholds.approval", "abc", true},
		{"learning.graduation_n", "1", false},
		{"learning.graduation_n", "10", false},
		{"learning.graduation_n", "-1", true},
		{"learning.graduation_n", "0", true},
		{"learning.graduation_n", "11", true},
		{"learning.graduation_n", "7", false},
		{"limits.max_error_retries", "3", false},
		{"llm.timeout_seconds", "90", false},
		{"llm.auto_act_confidence_threshold", "70", false},
		{"llm.auto_act_confidence_threshold", "-1", true},
		{"llm.auto_act_confidence_threshold", "maybe", true},
		{"llm.command", `claude -p "decide for me"`, false},
		{"llm.command_start", `claude -p "first: {agent_name}" --model opus`, false},
		{"llm.enable_rewrite_action", "true", false},
		{"llm.enable_rewrite_action", "maybe", true},
		{"llm.rewrite_action_fallback_template", "Act on: {original_text}", false},
		{"llm.task_generate_command", `claude -p "suggest: {agent_name}" --model haiku`, false},
		{"llm.task_generate_command_start", `claude -p "first suggest: {agent_name}"`, false},
		{"nonexistent.field", "1", true},
	}
	for _, c := range cases {
		err := app.SetField(ctx, c.key, c.value)
		if (err != nil) != c.wantErr {
			t.Errorf("SetField(%s, %s) error = %v, wantErr %v", c.key, c.value, err, c.wantErr)
		}
	}

	cfg, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConfidenceThresholds.Approval != 0.92 || cfg.Learning.GraduationN != 7 ||
		cfg.Limits.MaxErrorRetries != 3 || cfg.LLM.TimeoutSeconds != 90 ||
		cfg.LLM.AutoActConfidenceThreshold != 70 {
		t.Errorf("persisted config mismatch: %+v", cfg)
	}
	if len(cfg.LLM.Command) != 3 || cfg.LLM.Command[2] != "decide for me" {
		t.Errorf("llm.command quote handling: %v", cfg.LLM.Command)
	}
	if len(cfg.LLM.CommandStart) != 5 || cfg.LLM.CommandStart[2] != "first: {agent_name}" {
		t.Errorf("llm.command_start quote handling: %v", cfg.LLM.CommandStart)
	}
	if len(cfg.LLM.GenerateTaskCommand) != 5 || cfg.LLM.GenerateTaskCommand[2] != "suggest: {agent_name}" {
		t.Errorf("llm.task_generate_command quote handling: %v", cfg.LLM.GenerateTaskCommand)
	}
	if len(cfg.LLM.GenerateTaskCommandStart) != 3 || cfg.LLM.GenerateTaskCommandStart[2] != "first suggest: {agent_name}" {
		t.Errorf("llm.task_generate_command_start quote handling: %v", cfg.LLM.GenerateTaskCommandStart)
	}
	// Empty start variants render an inherit placeholder, not a blank cell.
	if got := frontend.FieldValue(config.Config{}, "llm.command_start"); got != "(inherits command)" {
		t.Errorf("empty command_start display = %q, want inherit placeholder", got)
	}
	if got := frontend.FieldValue(config.Config{}, "llm.task_generate_command_start"); got != "(inherits task_generate_command)" {
		t.Errorf("empty task_generate_command_start display = %q, want inherit placeholder", got)
	}
	if got := frontend.FieldValue(config.Config{}, "llm.task_generate_timeout_seconds"); got != "(inherits timeout_seconds)" {
		t.Errorf("empty task_generate_timeout_seconds display = %q, want inherit placeholder", got)
	}
	if !cfg.LLM.EnableRewriteAction ||
		cfg.LLM.RewriteActionFallbackTemplate != "Act on: {original_text}" {
		t.Errorf("rewrite-action keys not persisted: %+v", cfg.LLM)
	}
	// Every editable key renders a value.
	for _, key := range frontend.ConfigFieldKeys {
		if frontend.FieldValue(cfg, key) == "" {
			t.Errorf("FieldValue(%s) is empty", key)
		}
	}
}

// TestConfigFieldRegistryParity is the CR-033 three-way guarantee: every key
// in the ConfigFields registry must (1) render a display value from the
// default config and (2) be accepted by SetField with a valid sample value.
// Adding a key to the registry without teaching FieldValue/SetField about it
// (or without a sample here) fails this test by name.
func TestConfigFieldRegistryParity(t *testing.T) {
	// One valid sample value per registry key. When you add a field to
	// frontend.ConfigFields, add a sample here or this test fails loudly.
	samples := map[string]string{
		"confidence_thresholds.minimum":            "0.55",
		"confidence_thresholds.idle":               "0.70",
		"confidence_thresholds.approval":           "0.85",
		"confidence_thresholds.choice":             "0.85",
		"confidence_thresholds.error":              "0.90",
		"learning.graduation_n":                    "5",
		"learning.confirmation_weight":             "2.5",
		"embedding.pane_salient_chars":             "800",
		"limits.max_consecutive_auto_prompts":      "5",
		"limits.max_auto_prompts_per_minute":       "10",
		"limits.max_error_retries":                 "2",
		"safety.disable_never_auto_seed_patterns":  "true",
		"llm.command":                              `claude -p "decide"`,
		"llm.command_start":                        `claude -p "first: decide"`,
		"llm.timeout_seconds":                      "60",
		"llm.auto_act_confidence_threshold":        "70",
		"llm.pane_excerpt_chars":                   "4000",
		"llm.enable_rewrite_action":                "true",
		"llm.rewrite_action_fallback_template":     "Act on: {original_text}",
		"llm.task_generate_command":                `claude -p "suggest a task"`,
		"llm.task_generate_command_start":          `claude -p "first suggest a task"`,
		"llm.task_generate_timeout_seconds":        "45",
		"llm.env_file":                             "/etc/hap/llm.env",
		"llm.command_env_file":                     "/etc/hap/consult.env",
		"llm.command_start_env_file":               "/etc/hap/start.env",
		"llm.task_generate_command_env_file":       "/etc/hap/taskgen.env",
		"llm.task_generate_command_start_env_file": "/etc/hap/taskgen_start.env",
		"embedding.disabled":                       "false",
		"embedding.model_path":                     "/models/custom.gguf",
		"embedding.similarity_threshold":           "0.90",
		"embedding.bm25_min_score":                 "0.35",
		"embedding.model_context_window":           "512",
		"embedding.embed_timeout_ms":               "8000",
		"embedding.warm_timeout_ms":                "120000",
		"tui.max_content_width":                    "140",
		"tui.max_content_height":                   "12",
		"tui.theme":                                "dark",
		"tui.terminal_bell":                        "true",
		"cli.ai_agent_friendly_output":             "false",
	}

	registry := make(map[string]bool, len(frontend.ConfigFieldKeys))
	for _, key := range frontend.ConfigFieldKeys {
		registry[key] = true
		if _, ok := samples[key]; !ok {
			t.Errorf("registry key %q has no sample value in this test — add one to keep the CR-033 parity guarantee", key)
		}
	}
	for key := range samples {
		if !registry[key] {
			t.Errorf("sample key %q is not in frontend.ConfigFields — stale sample or missing registry entry", key)
		}
	}
	if t.Failed() {
		t.FailNow()
	}

	// (1) Every key renders a non-empty display value from the defaults.
	def := config.Default()
	for _, key := range frontend.ConfigFieldKeys {
		if frontend.FieldValue(def, key) == "" {
			t.Errorf("FieldValue(Default(), %s) is empty — FieldValue is missing the key", key)
		}
	}

	// (2) SetField accepts a valid sample for every key.
	app, _ := testApp(t)
	ctx := context.Background()
	for _, key := range frontend.ConfigFieldKeys {
		if err := app.SetField(ctx, key, samples[key]); err != nil {
			t.Errorf("SetField(%s, %q) rejected a valid value: %v", key, samples[key], err)
		}
	}
}

func TestAutoActConfidenceThresholdFieldDisplay(t *testing.T) {
	// The default (99) is a reachable 0-100 threshold, so it renders as a bare
	// number, not the "never" label.
	def := config.Default()
	got := frontend.FieldValue(def, "llm.auto_act_confidence_threshold")
	if got != "99" {
		t.Errorf("default threshold display = %q, want a bare 99", got)
	}
	// A value above 100 still renders with the "never" label.
	def.LLM.AutoActConfidenceThreshold = 999
	if got := frontend.FieldValue(def, "llm.auto_act_confidence_threshold"); !strings.Contains(got, "never") || !strings.Contains(got, "999") {
		t.Errorf("over-100 threshold should show a never label, got %q", got)
	}
	// A reachable 0-100 threshold renders plainly.
	def.LLM.AutoActConfidenceThreshold = 70
	if got := frontend.FieldValue(def, "llm.auto_act_confidence_threshold"); got != "70" {
		t.Errorf("in-range threshold display = %q, want 70", got)
	}

	// SetField round-trips and rejects negatives; 0 is a valid value.
	app, _ := testApp(t)
	ctx := context.Background()
	if err := app.SetField(ctx, "llm.auto_act_confidence_threshold", "0"); err != nil {
		t.Fatalf("threshold 0 (act on any score) must be accepted: %v", err)
	}
	cfg, _ := app.Config()
	if cfg.LLM.AutoActConfidenceThreshold != 0 {
		t.Errorf("SetField did not persist 0, got %d", cfg.LLM.AutoActConfidenceThreshold)
	}
	if err := app.SetField(ctx, "llm.auto_act_confidence_threshold", "-5"); err == nil {
		t.Error("negative threshold must be rejected")
	}
}

func TestPaneSalientCharsFieldDisplay(t *testing.T) {
	// Unset (0) renders the effective built-in default, not a bare "0".
	def := config.Default()
	got := frontend.FieldValue(def, "embedding.pane_salient_chars")
	if !strings.Contains(got, "default") || !strings.Contains(got, "500") {
		t.Errorf("unset pane_salient_chars should show the default, got %q", got)
	}
	// An explicit value renders plainly.
	def.Embedding.PaneSalientChars = 1200
	if got := frontend.FieldValue(def, "embedding.pane_salient_chars"); got != "1200" {
		t.Errorf("explicit pane_salient_chars display = %q, want 1200", got)
	}

	// SetField round-trips through the store and rejects non-positive values.
	app, _ := testApp(t)
	ctx := context.Background()
	if err := app.SetField(ctx, "embedding.pane_salient_chars", "1000"); err != nil {
		t.Fatal(err)
	}
	cfg, _ := app.Config()
	if cfg.Embedding.PaneSalientChars != 1000 {
		t.Errorf("SetField did not persist pane_salient_chars, got %d", cfg.Embedding.PaneSalientChars)
	}
	if err := app.SetField(ctx, "embedding.pane_salient_chars", "0"); err == nil {
		t.Error("pane_salient_chars must reject 0 (use omission for the default)")
	}
}

// TestFieldTUIEditableClassification pins CR-036: free-text fields (argv
// templates, template strings, paths) are read-only in the TUI, everything
// else in the registry is editable, and unknown keys are never editable.
func TestFieldTUIEditableClassification(t *testing.T) {
	readOnly := map[string]bool{
		"llm.command":                          true,
		"llm.command_start":                    true,
		"llm.rewrite_action_fallback_template": true,
		"llm.task_generate_command":            true,
		"llm.task_generate_command_start":      true,
		"embedding.model_path":                 true,
	}
	for _, key := range frontend.ConfigFieldKeys {
		want := !readOnly[key]
		if got := frontend.FieldTUIEditable(key); got != want {
			t.Errorf("FieldTUIEditable(%s) = %v, want %v", key, got, want)
		}
	}
	// Every expected read-only key must actually exist in the registry.
	present := make(map[string]bool, len(frontend.ConfigFieldKeys))
	for _, key := range frontend.ConfigFieldKeys {
		present[key] = true
	}
	for key := range readOnly {
		if !present[key] {
			t.Errorf("expected read-only key %q missing from ConfigFields", key)
		}
	}
	if frontend.FieldTUIEditable("nonexistent.field") {
		t.Error("unknown key must not be TUI-editable")
	}
}

func TestSetFieldNewKeysValidation(t *testing.T) {
	app, _ := testApp(t)
	ctx := context.Background()

	cases := []struct {
		key, value string
		wantErr    bool
	}{
		{"tui.theme", "dark", false},
		{"tui.theme", "solarized", true},
		{"tui.max_content_width", "140", false},
		{"tui.max_content_width", "0", false},
		{"tui.max_content_width", "-1", true},
		{"tui.max_content_width", "abc", true},
		{"tui.max_content_height", "12", false},
		{"tui.max_content_height", "0", false},
		{"tui.max_content_height", "-1", true},
		{"tui.max_content_height", "abc", true},
		{"safety.disable_never_auto_seed_patterns", "true", false},
		{"safety.disable_never_auto_seed_patterns", "false", false},
		{"safety.disable_never_auto_seed_patterns", "yes", true},
		{"llm.pane_excerpt_chars", "0", false}, // 0 = restore-default sentinel (fillZeroes)
		{"llm.pane_excerpt_chars", "-5", true},
		{"llm.pane_excerpt_chars", "abc", true},
		{"llm.pane_excerpt_chars", "4000", false},
		{"llm.task_generate_timeout_seconds", "0", false}, // 0 = inherit timeout_seconds
		{"llm.task_generate_timeout_seconds", "45", false},
		{"llm.task_generate_timeout_seconds", "-5", true},
		{"llm.task_generate_timeout_seconds", "abc", true},
		{"llm.task_generate_command", `claude -p "suggest"`, false},
		{"llm.task_generate_command", "", false}, // empty disables the feature
		{"llm.task_generate_command_start", `claude -p "first suggest"`, false},
		{"llm.task_generate_command_start", "", false}, // empty inherits task_generate_command
	}
	for _, c := range cases {
		err := app.SetField(ctx, c.key, c.value)
		if (err != nil) != c.wantErr {
			t.Errorf("SetField(%s, %q) error = %v, wantErr %v", c.key, c.value, err, c.wantErr)
		}
	}

	// The unknown-theme error names the valid themes.
	err := app.SetField(ctx, "tui.theme", "solarized")
	if err == nil {
		t.Fatal("unknown theme must be rejected")
	}
	for _, name := range config.ValidThemes {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("theme error should list valid name %q, got: %v", name, err)
		}
	}

	// Case-insensitive theme names normalize to lowercase on persist.
	if err := app.SetField(ctx, "tui.theme", "DARK"); err != nil {
		t.Fatalf("SetField(tui.theme, DARK) should normalize, got %v", err)
	}
	cfg, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TUI.Theme != "dark" {
		t.Errorf("persisted theme = %q, want normalized \"dark\"", cfg.TUI.Theme)
	}
	// Empty resets to the default theme.
	if err := app.SetField(ctx, "tui.theme", ""); err != nil {
		t.Fatalf("empty theme should reset: %v", err)
	}
	if cfg, _ = app.Config(); cfg.TUI.Theme != "" {
		t.Errorf("empty theme should persist as \"\", got %q", cfg.TUI.Theme)
	}

	// End each key on a NON-zero accepted value so persistence is positively
	// asserted (a validator that forgot the assignment would otherwise pass).
	if err := app.SetField(ctx, "tui.max_content_width", "140"); err != nil {
		t.Fatal(err)
	}
	if err := app.SetField(ctx, "tui.max_content_height", "12"); err != nil {
		t.Fatal(err)
	}
	if err := app.SetField(ctx, "safety.disable_never_auto_seed_patterns", "true"); err != nil {
		t.Fatal(err)
	}
	if err := app.SetField(ctx, "llm.task_generate_timeout_seconds", "30"); err != nil {
		t.Fatal(err)
	}
	if cfg, err = app.Config(); err != nil {
		t.Fatal(err)
	}
	if cfg.TUI.MaxContentWidth != 140 {
		t.Errorf("tui.max_content_width = %d, want 140", cfg.TUI.MaxContentWidth)
	}
	if cfg.TUI.MaxContentHeight != 12 {
		t.Errorf("tui.max_content_height = %d, want 12", cfg.TUI.MaxContentHeight)
	}
	if !cfg.Safety.DisableNeverAutoSeedPatterns {
		t.Error("safety.disable_never_auto_seed_patterns = false, want true (assignment not persisted)")
	}
	if cfg.LLM.PaneExcerptChars != 4000 {
		t.Errorf("llm.pane_excerpt_chars = %d, want 4000", cfg.LLM.PaneExcerptChars)
	}
	if cfg.LLM.GenerateTaskTimeoutSeconds != 30 {
		t.Errorf("llm.task_generate_timeout_seconds = %d, want 30 (assignment not persisted)", cfg.LLM.GenerateTaskTimeoutSeconds)
	}
}

func TestSplitCommand(t *testing.T) {
	cases := []struct {
		in      string
		want    []string
		wantErr bool
	}{
		{`a b c`, []string{"a", "b", "c"}, false},
		{`claude --mcp-config '{"x":1}' -p "hello world"`, []string{"claude", "--mcp-config", `{"x":1}`, "-p", "hello world"}, false},
		{``, nil, false},
		{`"unterminated`, nil, true},
	}
	for _, c := range cases {
		got, err := frontend.SplitCommand(c.in)
		if (err != nil) != c.wantErr {
			t.Fatalf("SplitCommand(%q) err = %v", c.in, err)
		}
		if !c.wantErr && fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("SplitCommand(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestRemoveByIndexIsValueVerified(t *testing.T) {
	app, _ := testApp(t)
	ctx := context.Background()
	app.AddNeverAutoPattern(ctx, `(?i)one`)
	app.AddNeverAutoPattern(ctx, `(?i)two`)
	app.AddTaskSource(ctx, "builder", "", "/tmp/tasks.md", "")

	// A stale expectation must refuse to delete (safety-relevant: never
	// silently remove the wrong never-auto pattern).
	if err := app.RemoveNeverAutoPattern(ctx, 0, `(?i)two`); err == nil {
		t.Fatal("mismatched expected pattern must refuse removal")
	}
	if err := app.RemoveNeverAutoPattern(ctx, 0, `(?i)one`); err != nil {
		t.Fatal(err)
	}
	cfg, _ := app.Config()
	if len(cfg.Safety.NeverAutoPatterns) != 1 || cfg.Safety.NeverAutoPatterns[0] != `(?i)two` {
		t.Errorf("wrong pattern removed: %v", cfg.Safety.NeverAutoPatterns)
	}
	if err := app.RemoveNeverAutoPattern(ctx, 5, "x"); err == nil {
		t.Error("out-of-range pattern index must error")
	}

	listed := config.TaskSource{Agent: "builder", Path: "/tmp/tasks.md", MaxTasks: config.DefaultMaxTasks}
	if err := app.RemoveTaskSource(ctx, 0, config.TaskSource{Agent: "builder", Path: "/wrong/path.md"}); err == nil {
		t.Error("mismatched expected path must refuse removal")
	}
	if err := app.RemoveTaskSource(ctx, 0, config.TaskSource{Agent: "someone-else", Path: "/tmp/tasks.md"}); err == nil {
		t.Error("mismatched agent selector must refuse removal")
	}
	if err := app.RemoveTaskSource(ctx, 0, listed); err != nil {
		t.Fatal(err)
	}
	if err := app.RemoveTaskSource(ctx, 0, listed); err == nil {
		t.Error("removing from empty task sources must error")
	}
}

func TestJoinCommandRoundTrip(t *testing.T) {
	// Display → edit → save must never corrupt llm.command (a no-op edit in
	// the TUI re-parses the rendered string).
	cases := [][]string{
		{"claude", "-p", "decide for me"},
		{"llm", "--json", `{"a":1}`},
		{"cmd", "it's fine"},
		{"plain", "args", "only"},
		{"empty-arg", ""},
	}
	for _, argv := range cases {
		rendered := frontend.JoinCommand(argv)
		back, err := frontend.SplitCommand(rendered)
		if err != nil {
			t.Fatalf("SplitCommand(JoinCommand(%q)) error: %v", argv, err)
		}
		if fmt.Sprint(back) != fmt.Sprint(argv) {
			t.Errorf("round trip changed argv: %q → %q → %q", argv, rendered, back)
		}
	}
}

// fakeHerdrPort serves a fixed live agent list (no sends expected).
type fakeHerdrPort struct {
	agents []domain.AgentTransition
}

func (f *fakeHerdrPort) Send(ctx context.Context, paneID, input string) error { return nil }
func (f *fakeHerdrPort) ReadPane(ctx context.Context, paneID string, lines int) (string, error) {
	return "", nil
}
func (f *fakeHerdrPort) ListAgents(ctx context.Context) ([]domain.AgentTransition, error) {
	return f.agents, nil
}

func TestStatusHidesOnlyDoublePlaceholderAgents(t *testing.T) {
	app, _ := testApp(t)
	app.Herdr = &fakeHerdrPort{agents: []domain.AgentTransition{
		{AgentID: "panel", PaneID: "panel", AgentType: "undefined", Status: "unknown"},
		{AgentID: "real-unknown-status", PaneID: "real-unknown-status", AgentType: "claude", Status: "unknown"},
		{AgentID: "active-unknown-type", PaneID: "active-unknown-type", AgentType: "undefined", Status: "working"},
	}}

	status, err := app.GetStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.MonitoredAgents) != 2 {
		t.Fatalf("TUI agents = %+v, want two non-double-placeholder rows", status.MonitoredAgents)
	}
	if status.MonitoredAgents[0].AgentID != "real-unknown-status" ||
		status.MonitoredAgents[1].AgentID != "active-unknown-type" {
		t.Fatalf("wrong agents remained visible: %+v", status.MonitoredAgents)
	}
}

func TestCaptureAgentResolvesNameAndNudgesDaemon(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	if err := st.AssignAgentName(ctx, "pane-2", "vivid-falcon"); err != nil {
		t.Fatal(err)
	}
	app.Herdr = &fakeHerdrPort{agents: []domain.AgentTransition{
		{AgentID: "pane-1", PaneID: "pane-1", AgentType: "codex", Status: "working"},
		{AgentID: "pane-2", PaneID: "pane-2", AgentType: "codex", Status: "blocked"},
	}}
	sock := filepath.Join(testutil.SocketDir(t), "capture.sock")
	got := make(chan control.Kind, 1)
	srv, err := control.NewServer(sock, func(k control.Kind) { got <- k })
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	app.ControlPath = sock

	agent, err := app.CaptureAgent(ctx, "vivid-falcon")
	if err != nil {
		t.Fatal(err)
	}
	if agent.AgentID != "pane-2" || agent.Status != "blocked" {
		t.Fatalf("resolved agent = %+v", agent)
	}
	select {
	case kind := <-got:
		if target, ok := control.CaptureTarget(kind); !ok || target != "pane-2" {
			t.Fatalf("capture target = %q, %v", target, ok)
		}
	case <-time.After(time.Second):
		t.Fatal("capture nudge not received")
	}

	if _, err := app.CaptureAgent(ctx, "pane-1"); err == nil || !strings.Contains(err.Error(), "is working") {
		t.Fatalf("working agent must be rejected, got %v", err)
	}
	if _, err := app.CaptureAgent(ctx, "missing"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing agent must be rejected, got %v", err)
	}
}

func TestRenameLiveButUnnamedAgent(t *testing.T) {
	// Regression: the TUI/CLI list agents straight from Herdr, but the
	// daemon only creates a name row when the agent first transitions. A
	// live agent with no row yet ("no agent known as ...") must still be
	// renamable — the rename verifies liveness and creates the row.
	app, _ := testApp(t)
	ctx := context.Background()
	app.Herdr = &fakeHerdrPort{agents: []domain.AgentTransition{
		{AgentID: "w65:p1", PaneID: "w65:p1", AgentType: "claude", Status: "blocked"},
	}}

	if err := app.RenameAgent(ctx, "w65:p1", "quiet-agent"); err != nil {
		t.Fatalf("renaming a live unnamed agent must succeed: %v", err)
	}
	names, _ := app.Names(ctx)
	if names["w65:p1"] != "quiet-agent" {
		t.Fatalf("name row not created: %v", names)
	}

	// A target that is neither named nor live must still be rejected.
	if err := app.RenameAgent(ctx, "w99:p9", "ghost"); err == nil {
		t.Error("renaming a non-live unknown agent must fail")
	}
}

func TestRenameAgentThroughApp(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	// The daemon names agents on first sight; simulate that.
	auto, err := st.EnsureAgentName(ctx, "w3:p1")
	if err != nil || auto == "" {
		t.Fatalf("ensure: %q %v", auto, err)
	}
	if err := app.RenameAgent(ctx, auto, "backend-dev"); err != nil {
		t.Fatal(err)
	}
	names, err := app.Names(ctx)
	if err != nil || names["w3:p1"] != "backend-dev" {
		t.Fatalf("rename not visible: %v %v", names, err)
	}
	st2, _ := app.GetStatus(ctx)
	if st2.AgentName("w3:p1") != "backend-dev" {
		t.Error("status should carry agent names")
	}
	if err := app.RenameAgent(ctx, "no-such-agent", "x"); err == nil {
		t.Error("renaming an unknown agent must fail")
	}
}

// TestCLIParityWithSharedLayer proves FR-022: every CLI verb operates on the
// same shared state the TUI reads.
func TestCLIParityWithSharedLayer(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()

	run := func(verb string, args ...string) string {
		t.Helper()
		var buf bytes.Buffer
		if err := cli.Run(ctx, app, &buf, verb, args); err != nil {
			t.Fatalf("cli %s: %v", verb, err)
		}
		return buf.String()
	}

	// pause via CLI → visible in shared status
	run("pause")
	stat, _ := app.GetStatus(ctx)
	if !stat.Paused {
		t.Fatal("CLI pause must hit the shared state")
	}
	if !strings.Contains(run("status"), "PAUSED") {
		t.Error("status should show paused")
	}
	run("resume")

	// escalation → confirm via CLI
	id, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "a1", SituationType: domain.SituationApproval, Trigger: "t",
		Action: "escalated", Status: "escalated", Suggestion: "respond: y", CreatedAt: time.Now(),
	})
	if !strings.Contains(run("escalations"), "respond: y") {
		t.Error("escalations should list the suggestion")
	}
	run("confirm", fmt.Sprintf("%d", id))
	corr, _ := st.UnprocessedCorrections(ctx)
	if len(corr) != 1 || corr[0].CorrectedAction != "y" {
		t.Fatalf("CLI confirm must record the learning event: %+v", corr)
	}

	// threshold mutation via CLI verb → visible via shared config
	run("config", "set-threshold", "choice", "0.9")
	cfg, _ := app.Config()
	if cfg.ConfidenceThresholds.Choice != 0.9 {
		t.Error("CLI threshold edit must land in shared config")
	}

	// audit + kill-history + rules render without error
	if out := run("audit"); !strings.Contains(out, "escalated") {
		t.Errorf("audit output: %q", out)
	}
	if out := run("kill-history"); !strings.Contains(out, "active") {
		t.Errorf("kill history output: %q", out)
	}
	if out := run("rules", "list"); !strings.Contains(out, "seed strict") || !strings.Contains(out, "seed heuristic") {
		t.Errorf("rules output: %q", out)
	}
	run("config", "set", "safety.disable_never_auto_seed_patterns", "true")
	if out := run("rules", "list"); !strings.Contains(out, "shipped never-auto rules disabled") ||
		strings.Contains(out, "seed strict") || strings.Contains(out, "seed heuristic") {
		t.Errorf("disabled rules output: %q", out)
	}

	// generic field editor via CLI verb → shared config
	run("config", "set", "learning.graduation_n", "8")
	cfg, _ = app.Config()
	if cfg.Learning.GraduationN != 8 {
		t.Error("config set must land in shared config")
	}
	if out := run("config", "fields"); !strings.Contains(out, "safety.disable_never_auto_seed_patterns") ||
		strings.Contains(out, "safety.disable_seed") || strings.Contains(out, "limits.verify_unblock_ms") {
		t.Errorf("config fields output contains stale or missing keys: %q", out)
	}

	// rules add/remove round trip
	run("rules", "add", `(?i)reboot\s+router`)
	run("rules", "remove", "0")
	cfg, _ = app.Config()
	if len(cfg.Safety.NeverAutoPatterns) != 0 {
		t.Errorf("rules remove failed: %v", cfg.Safety.NeverAutoPatterns)
	}

	// task-source add/list/remove round trip
	run("task-source", "add", "--agent", "builder", "/tmp/tasks.md")
	if out := run("task-source", "list"); !strings.Contains(out, "builder") {
		t.Errorf("task-source list output: %q", out)
	}
	run("task-source", "remove", "0")
	cfg, _ = app.Config()
	if len(cfg.TaskSources) != 0 {
		t.Errorf("task-source remove failed: %v", cfg.TaskSources)
	}

	// rename via CLI verb → shared names
	auto, err := st.EnsureAgentName(ctx, "w1:p1")
	if err != nil || auto == "" {
		t.Fatalf("ensure name: %q %v", auto, err)
	}
	run("rename", auto, "frontend-dev")
	names, _ := app.Names(ctx)
	if names["w1:p1"] != "frontend-dev" {
		t.Errorf("CLI rename must hit shared state: %v", names)
	}
}

func TestSignaturesEnrichmentAndFilter(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	now := time.Now()
	st.UpsertSignature(ctx, domain.SignatureState{
		Signature: "approval:one", SituationType: domain.SituationApproval, AgentType: "claude",
		Mode: domain.ModeShadow, CachedConfidence: 0.6, UpdatedAt: now,
	})
	st.UpsertSignature(ctx, domain.SignatureState{
		Signature: "choice:two", SituationType: domain.SituationChoice, AgentType: "codex",
		Mode: domain.ModeAutonomous, CachedConfidence: 0.95, UpdatedAt: now.Add(time.Second),
	})
	for i := 0; i < 3; i++ {
		st.RecordDecision(ctx, domain.DecisionRecord{Signature: "approval:one",
			SituationType: domain.SituationApproval, AgentType: "claude",
			ChosenAction: "1", Source: domain.SourceOperator, CreatedAt: now})
	}
	st.RecordDecision(ctx, domain.DecisionRecord{Signature: "approval:one",
		SituationType: domain.SituationApproval, AgentType: "claude",
		ChosenAction: "2", Source: domain.SourceOperator, CreatedAt: now})

	rows, err := app.Signatures(ctx, domain.SignatureFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Signature != "choice:two" {
		t.Fatalf("want 2 rows newest first, got %+v", rows)
	}
	one := rows[1]
	if one.TopAction != "1" || one.Decisions != 4 {
		t.Errorf("enrichment: top=%q n=%d, want top=1 n=4", one.TopAction, one.Decisions)
	}

	// Filter pass-through.
	rows, err = app.Signatures(ctx, domain.SignatureFilter{Mode: domain.ModeAutonomous})
	if err != nil || len(rows) != 1 || rows[0].Signature != "choice:two" {
		t.Errorf("mode filter: got %+v, %v", rows, err)
	}
}

func TestSignaturesRenderLiveConfidenceNotCachedSnapshot(t *testing.T) {
	// The Rules tab CONF column, `hap signatures`, and the escalation rule line
	// all render SignatureRow.Confidence. It must be the score the decision core
	// gates on RIGHT NOW, recomputed from history — never the persisted
	// CachedConfidence, which is refreshed only on a confirm/correct and so
	// drifts as ordinary decisions accumulate. The live symptom: a rule the core
	// scored 0.45 displayed "confidence 1.00" beside its own
	// "[variance_guard] contradictory history" escalation.
	app, st := testApp(t)
	ctx := context.Background()
	now := time.Now()
	st.UpsertSignature(ctx, domain.SignatureState{
		Signature: "approval:drift", SituationType: domain.SituationApproval,
		AgentType: "claude", Mode: domain.ModeShadow,
		CachedConfidence: 1.0, // a stale snapshot from an earlier, unanimous moment
		UpdatedAt:        now,
	})
	// Contradictory history: recency-weighted agreement lands near 0.54.
	for _, a := range []string{"y", "n", "y", "n"} {
		st.RecordDecision(ctx, domain.DecisionRecord{Signature: "approval:drift",
			SituationType: domain.SituationApproval, AgentType: "claude",
			ChosenAction: a, CreatedAt: now})
	}

	rows, err := app.Signatures(ctx, domain.SignatureFilter{})
	if err != nil || len(rows) != 1 {
		t.Fatalf("Signatures: %+v, %v", rows, err)
	}
	if got := rows[0].Confidence; got <= 0.50 || got >= 0.60 {
		t.Errorf("confidence must be recomputed live (~0.54), got %.4f", got)
	}
	if rows[0].Confidence == rows[0].CachedConfidence {
		t.Error("confidence must not be the stale cached snapshot")
	}
	// RuleSummary feeds the escalation line the operator reads.
	if s := frontend.RuleSummary(rows[0], 3); !strings.Contains(s, "confidence 0.54") {
		t.Errorf("rule summary must quote the live score, got %q", s)
	}

	// A RESET rule is the sharpest case: ResetGraduation stamps a fake 1.0 and
	// the floor excludes every decision, so the row must read as "no post-reset
	// evidence yet" (0.00) while still naming the action it learned.
	hist, err := st.DecisionsForSignature(ctx, "approval:drift", 50)
	if err != nil || len(hist) == 0 {
		t.Fatalf("history: %+v, %v", hist, err)
	}
	st.UpsertSignature(ctx, domain.SignatureState{
		Signature: "approval:drift", SituationType: domain.SituationApproval,
		AgentType: "claude", Mode: domain.ModeShadow,
		CachedConfidence: 1.0,
		DecisionFloorID:  hist[0].ID, // newest id: nothing survives the floor
		UpdatedAt:        now,
	})
	rows, err = app.Signatures(ctx, domain.SignatureFilter{})
	if err != nil || len(rows) != 1 {
		t.Fatalf("Signatures after reset: %+v, %v", rows, err)
	}
	if rows[0].Confidence != 0 || rows[0].Decisions != 0 {
		t.Errorf("a reset rule has no post-floor evidence: conf=%.2f n=%d",
			rows[0].Confidence, rows[0].Decisions)
	}
	if rows[0].TopAction == "" {
		t.Error("a reset rule must still name its learned action (full-history fallback)")
	}
}

func TestSignaturesMinConfidenceFiltersLiveScoreBothDirections(t *testing.T) {
	// --min-conf must select on the LIVE score. The stale cached snapshot drifts
	// BOTH ways, so a SQL filter on it fails in both: it would drop a
	// live-confident rule (cached low) and keep a contradictory one (cached
	// high) that visibly renders below the cutoff.
	app, st := testApp(t)
	ctx := context.Background()
	now := time.Now()
	// Live 1.00 (unanimous) but cached far below the cutoff — must be KEPT.
	st.UpsertSignature(ctx, domain.SignatureState{
		Signature: "approval:livehigh", SituationType: domain.SituationApproval,
		AgentType: "claude", Mode: domain.ModeShadow,
		CachedConfidence: 0.10, UpdatedAt: now,
	})
	for i := 0; i < 3; i++ {
		st.RecordDecision(ctx, domain.DecisionRecord{Signature: "approval:livehigh",
			SituationType: domain.SituationApproval, ChosenAction: "y", CreatedAt: now})
	}
	// Live ~0.54 (contradictory) but cached 1.00 — must be DROPPED.
	st.UpsertSignature(ctx, domain.SignatureState{
		Signature: "approval:livelow", SituationType: domain.SituationApproval,
		AgentType: "claude", Mode: domain.ModeShadow,
		CachedConfidence: 1.00, UpdatedAt: now.Add(time.Second),
	})
	for _, a := range []string{"y", "n", "y", "n"} {
		st.RecordDecision(ctx, domain.DecisionRecord{Signature: "approval:livelow",
			SituationType: domain.SituationApproval, ChosenAction: a, CreatedAt: now})
	}

	rows, err := app.Signatures(ctx, domain.SignatureFilter{MinConfidence: 0.9})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Signature != "approval:livehigh" {
		t.Fatalf("min-conf must select on the live score, got %+v", rows)
	}
	// Nothing below the cutoff may survive — the operator-visible invariant.
	for _, r := range rows {
		if r.Confidence < 0.9 {
			t.Errorf("row %s renders %.2f, below the 0.90 cutoff", r.Signature, r.Confidence)
		}
	}
	// Sanity: without the filter both rules are listed.
	if all, err := app.Signatures(ctx, domain.SignatureFilter{}); err != nil || len(all) != 2 {
		t.Errorf("unfiltered listing should hold both: %+v, %v", all, err)
	}
}

func TestSignatureRowTotalDecisionsIgnoresResetFloor(t *testing.T) {
	// The delete prompts quote TotalDecisions because DeleteSignature erases
	// every row regardless of the floor. A RESET rule is the trap: Decisions
	// (post-floor, for the confidence line) is 0 while N rows still exist, so
	// quoting Decisions would confirm "delete ... and its 0 decision(s)" and
	// then destroy N.
	app, st := testApp(t)
	ctx := context.Background()
	now := time.Now()
	st.UpsertSignature(ctx, domain.SignatureState{
		Signature: "approval:reset", SituationType: domain.SituationApproval,
		AgentType: "claude", Mode: domain.ModeShadow, UpdatedAt: now,
	})
	for i := 0; i < 3; i++ {
		st.RecordDecision(ctx, domain.DecisionRecord{Signature: "approval:reset",
			SituationType: domain.SituationApproval, ChosenAction: "y", CreatedAt: now})
	}
	hist, err := st.DecisionsForSignature(ctx, "approval:reset", 50)
	if err != nil || len(hist) != 3 {
		t.Fatalf("seed history: %+v, %v", hist, err)
	}
	// Reset: floor above every decision, exactly like ResetGraduation stamps.
	st.UpsertSignature(ctx, domain.SignatureState{
		Signature: "approval:reset", SituationType: domain.SituationApproval,
		AgentType: "claude", Mode: domain.ModeShadow,
		CachedConfidence: 1.0, DecisionFloorID: hist[0].ID, UpdatedAt: now,
	})

	row, _, err := app.SignatureDetail(ctx, "approval:reset")
	if err != nil {
		t.Fatal(err)
	}
	if row.Decisions != 0 {
		t.Errorf("post-floor Decisions should be 0, got %d", row.Decisions)
	}
	if row.TotalDecisions != 3 {
		t.Errorf("TotalDecisions must count every stored row, got %d", row.TotalDecisions)
	}
	// The prompt's count must match what the delete actually erases.
	_, n, err := app.DeleteSignature(ctx, "approval:reset")
	if err != nil {
		t.Fatal(err)
	}
	if int(n) != row.TotalDecisions {
		t.Errorf("delete erased %d decisions but the prompt would have quoted %d", n, row.TotalDecisions)
	}

	// Listing rows carry the same count.
	st.UpsertSignature(ctx, domain.SignatureState{
		Signature: "approval:live", SituationType: domain.SituationApproval,
		AgentType: "claude", Mode: domain.ModeShadow, UpdatedAt: now,
	})
	st.RecordDecision(ctx, domain.DecisionRecord{Signature: "approval:live",
		SituationType: domain.SituationApproval, ChosenAction: "y", CreatedAt: now})
	rows, err := app.Signatures(ctx, domain.SignatureFilter{})
	if err != nil || len(rows) != 1 {
		t.Fatalf("Signatures: %+v, %v", rows, err)
	}
	if rows[0].TotalDecisions != 1 {
		t.Errorf("listing row TotalDecisions = %d, want 1", rows[0].TotalDecisions)
	}
}

func TestConfidenceLabelDashWhenNeverScored(t *testing.T) {
	// 0.00 is unreachable as a real agreement score — it is topWeight/total over
	// a non-empty history, so it is always strictly positive. A stored 0 can
	// therefore only mean "the core never scored this", and rendering it as
	// "0.00" says the opposite: measured, and found no confidence.
	tests := []struct {
		name string
		conf float64
		want string
	}{
		{"never scored", 0, "-"},
		// Recency decay bounds the weight total, so a real score never lands
		// near zero: ~0.15 is about the floor and 0.24 is the lowest ever seen
		// in the wild. Nothing genuine gets close to rounding to "0.00".
		{"about the real floor", 0.15, "0.15"},
		{"lowest score actually observed in the wild", 0.24, "0.24"},
		{"contradictory but measured", 0.45, "0.45"},
		{"unanimous", 1, "1.00"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := frontend.ConfidenceLabel(tc.conf); got != tc.want {
				t.Errorf("ConfidenceLabel(%v) = %q, want %q", tc.conf, got, tc.want)
			}
		})
	}
}

func TestRuleSummaryShowsDashForUnscoredRule(t *testing.T) {
	// A rule reset to re-earn trust has no post-reset evidence, so its live
	// score is 0 — "confidence -" says "not scored yet", where "confidence 0.00"
	// would claim the rule was measured and found worthless.
	reset := signatureRowFor(domain.ModeShadow, 0, "1")
	if s := frontend.RuleSummary(reset, 3); !strings.Contains(s, "confidence -") {
		t.Errorf("a reset rule's summary must read \"confidence -\", got %q", s)
	}
	scored := signatureRowFor(domain.ModeShadow, 0.45, "1")
	if s := frontend.RuleSummary(scored, 3); !strings.Contains(s, "confidence 0.45") {
		t.Errorf("a measured rule keeps its number, got %q", s)
	}
}

// signatureRowFor builds a display row for rule-summary assertions.
func signatureRowFor(mode domain.Mode, conf float64, top string) frontend.SignatureRow {
	return frontend.SignatureRow{
		SignatureState: domain.SignatureState{
			Signature: "approval:x", SituationType: domain.SituationApproval,
			Mode: mode, ConsecutiveConfirmations: 1,
		},
		Confidence: conf, TopAction: top, Decisions: 0,
	}
}

func TestTotalDecisionsCountsBeyondTheHistoryWindow(t *testing.T) {
	// The delete prompts quote TotalDecisions, and DeleteSignature erases every
	// row with one unfiltered DELETE while nothing prunes the table — so a rule
	// outlives any read window. Deriving the count from the 50-row history slice
	// would confirm "and its 50 decision(s)" and then destroy 63.
	const total = 63
	app, st := testApp(t)
	ctx := context.Background()
	now := time.Now()
	st.UpsertSignature(ctx, domain.SignatureState{
		Signature: "approval:long", SituationType: domain.SituationApproval,
		AgentType: "claude", Mode: domain.ModeShadow, UpdatedAt: now,
	})
	for i := 0; i < total; i++ {
		if _, err := st.RecordDecision(ctx, domain.DecisionRecord{
			Signature: "approval:long", SituationType: domain.SituationApproval,
			ChosenAction: "y", CreatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}

	row, _, err := app.SignatureDetail(ctx, "approval:long")
	if err != nil {
		t.Fatal(err)
	}
	if row.TotalDecisions != total {
		t.Errorf("detail TotalDecisions = %d, want %d (the window would cap it at 50)", row.TotalDecisions, total)
	}
	rows, err := app.Signatures(ctx, domain.SignatureFilter{})
	if err != nil || len(rows) != 1 {
		t.Fatalf("Signatures: %+v, %v", rows, err)
	}
	if rows[0].TotalDecisions != total {
		t.Errorf("listing TotalDecisions = %d, want %d", rows[0].TotalDecisions, total)
	}
	// The count the prompt quotes must equal what the delete actually erases.
	_, deleted, err := app.DeleteSignature(ctx, "approval:long")
	if err != nil {
		t.Fatal(err)
	}
	if int(deleted) != row.TotalDecisions {
		t.Errorf("delete erased %d decisions but the prompt quoted %d", deleted, row.TotalDecisions)
	}
}

func TestSignatureDetail(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	now := time.Now()
	st.UpsertSignature(ctx, domain.SignatureState{
		Signature: "approval:detail", SituationType: domain.SituationApproval,
		AgentType: "claude", Mode: domain.ModeShadow, CachedConfidence: 0.7, UpdatedAt: now,
	})
	st.RecordDecision(ctx, domain.DecisionRecord{Signature: "approval:detail",
		SituationType: domain.SituationApproval, AgentType: "claude",
		ChosenAction: "yes", Source: domain.SourceOperator, CreatedAt: now})
	st.AppendAudit(ctx, domain.AuditRecord{Signature: "approval:detail",
		Trigger: "apply the diff?", SituationType: domain.SituationApproval,
		Action: "escalated", Rationale: "shadow mode", Status: "escalated", CreatedAt: now})

	row, history, err := app.SignatureDetail(ctx, "approval:det")
	if err != nil {
		t.Fatal(err)
	}
	if row.Signature != "approval:detail" || row.TopAction != "yes" || row.Decisions != 1 {
		t.Errorf("detail row: %+v", row)
	}
	if len(history) != 1 || history[0].ChosenAction != "yes" {
		t.Errorf("history: %+v", history)
	}
	if row.LastAudit == nil || row.LastAudit.Trigger != "apply the diff?" {
		t.Errorf("last audit: %+v", row.LastAudit)
	}

	if _, _, err := app.SignatureDetail(ctx, "nope"); err == nil {
		t.Error("unknown prefix must error")
	}
}

func TestDeleteSignatureThroughApp(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	now := time.Now()
	st.UpsertSignature(ctx, domain.SignatureState{
		Signature: "error:gone", SituationType: domain.SituationError,
		AgentType: "claude", Mode: domain.ModeShadow, UpdatedAt: now,
	})
	st.RecordDecision(ctx, domain.DecisionRecord{Signature: "error:gone",
		SituationType: domain.SituationError, AgentType: "claude",
		ChosenAction: "retry", Source: domain.SourceRule, CreatedAt: now})

	sig, n, err := app.DeleteSignature(ctx, "error:g")
	if err != nil {
		t.Fatal(err)
	}
	if sig != "error:gone" || n != 1 {
		t.Errorf("delete: sig=%q n=%d", sig, n)
	}
	if got, _ := st.GetSignature(ctx, "error:gone"); got != nil {
		t.Error("signature should be deleted")
	}
	if _, _, err := app.DeleteSignature(ctx, "error:g"); err == nil {
		t.Error("prefix resolution error must surface")
	}
}

func TestDeleteSignatureNudgesReload(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	st.UpsertSignature(ctx, domain.SignatureState{
		Signature: "idle:nudged", SituationType: domain.SituationIdle,
		AgentType: "claude", Mode: domain.ModeShadow, UpdatedAt: time.Now(),
	})

	got := make(chan control.Kind, 1)
	sock := filepath.Join(testutil.SocketDir(t), "ctl.sock")
	srv, err := control.NewServer(sock, func(k control.Kind) { got <- k })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	app.ControlPath = sock

	if _, _, err := app.DeleteSignature(ctx, "idle:n"); err != nil {
		t.Fatal(err)
	}
	select {
	case k := <-got:
		if k != control.KindReload {
			t.Errorf("nudge kind = %q, want reload", k)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("DeleteSignature must nudge the daemon with KindReload")
	}
}

func TestResetSignatureGraduationThroughApp(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	now := time.Now()
	st.UpsertSignature(ctx, domain.SignatureState{
		Signature: "approval:grad", SituationType: domain.SituationApproval,
		AgentType: "claude", Mode: domain.ModeAutonomous, ConsecutiveConfirmations: 9,
		CachedConfidence: 0.4, UpdatedAt: now,
	})
	var lastID int64
	for _, a := range []string{"1", "1"} {
		lastID, _ = st.RecordDecision(ctx, domain.DecisionRecord{Signature: "approval:grad",
			SituationType: domain.SituationApproval, AgentType: "claude",
			ChosenAction: a, Source: domain.SourceOperator, CreatedAt: now})
	}

	sig, err := app.ResetSignatureGraduation(ctx, "approval:g")
	if err != nil {
		t.Fatal(err)
	}
	if sig != "approval:grad" {
		t.Errorf("reset resolved sig = %q, want approval:grad", sig)
	}
	got, _ := st.GetSignature(ctx, "approval:grad")
	if got == nil || got.Mode != domain.ModeShadow || got.ConsecutiveConfirmations != 0 {
		t.Errorf("reset must return the signature to shadow with a zero streak: %+v", got)
	}
	// Reset clears confidence (fresh 1.0) and stamps the floor at the newest
	// decision id so pre-reset decisions stop counting.
	if got.CachedConfidence != 1.0 {
		t.Errorf("reset must set cached confidence to 1.0, got %.3f", got.CachedConfidence)
	}
	if got.DecisionFloorID != lastID {
		t.Errorf("reset floor = %d, want newest decision id %d", got.DecisionFloorID, lastID)
	}
	// Decision history is kept (a reset is not a delete).
	if decs, _ := st.DecisionsForSignature(ctx, "approval:grad", 10); len(decs) != 2 {
		t.Errorf("reset must keep decision history, got %d", len(decs))
	}
	// Unknown prefix surfaces the resolution error.
	if _, err := app.ResetSignatureGraduation(ctx, "nope:xyz"); err == nil {
		t.Error("prefix resolution error must surface")
	}
}

// fakeLocatorPort is a fakeHerdrPort that also reports workspace/tab
// metadata (ports.LocatorPort).
type fakeLocatorPort struct {
	fakeHerdrPort
	workspaces []domain.WorkspaceInfo
	tabs       []domain.TabInfo
}

func (f *fakeLocatorPort) ListWorkspaces(ctx context.Context) ([]domain.WorkspaceInfo, error) {
	return f.workspaces, nil
}
func (f *fakeLocatorPort) ListTabs(ctx context.Context) ([]domain.TabInfo, error) {
	return f.tabs, nil
}

func TestGetStatusNamesLiveAgentsAndReportsLocation(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	app.Herdr = &fakeLocatorPort{
		fakeHerdrPort: fakeHerdrPort{agents: []domain.AgentTransition{
			{AgentID: "w23:p5", PaneID: "w23:p5", TabID: "w23:t1", WorkspaceID: "w23",
				AgentType: "claude", Status: "working"},
		}},
		workspaces: []domain.WorkspaceInfo{{ID: "w23", Label: "backend", Number: 23}},
		tabs:       []domain.TabInfo{{ID: "w23:t1", Label: "1", Number: 1, WorkspaceID: "w23"}},
	}

	stat, err := app.GetStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// A live agent with no name row gets one immediately — the operator
	// never stares at a bare pane id.
	name := stat.AgentName("w23:p5")
	if name == "" || !strings.Contains(name, "-") {
		t.Fatalf("live agent should be auto-named with a two-word slug, got %q", name)
	}
	persisted, _ := st.AgentNames(ctx)
	if persisted["w23:p5"] != name {
		t.Error("auto-assigned name must be persisted")
	}
	// A second call is stable (insert-if-absent).
	stat2, _ := app.GetStatus(ctx)
	if stat2.AgentName("w23:p5") != name {
		t.Error("name must be stable across refreshes")
	}
	// Location metadata is exposed for the detail view.
	if ws := stat.Workspaces["w23"]; ws.Label != "backend" || ws.Number != 23 {
		t.Errorf("workspace metadata: %+v", stat.Workspaces)
	}
	if tab := stat.Tabs["w23:t1"]; tab.Number != 1 {
		t.Errorf("tab metadata: %+v", stat.Tabs)
	}
	// An operator rename is never clobbered by the auto-naming pass.
	if err := app.RenameAgent(ctx, "w23:p5", "backend-dev"); err != nil {
		t.Fatal(err)
	}
	stat3, _ := app.GetStatus(ctx)
	if stat3.AgentName("w23:p5") != "backend-dev" {
		t.Errorf("rename clobbered: %q", stat3.AgentName("w23:p5"))
	}
}

func TestConfirmNoopSuggestionSkipsSend(t *testing.T) {
	// Confirming a "do nothing" suggestion records the @noop learning event
	// and never writes anything to the pane — even with send=true.
	app, st := testApp(t)
	fake := &fakeHerdr{}
	app.Herdr = fake
	ctx := context.Background()

	id, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w1:p9", SituationType: domain.SituationIdle, Trigger: "t",
		Action: "escalated", Status: "escalated",
		Suggestion: "LLM suggested: " + domain.ActionNoopSuggestion, CreatedAt: time.Now(),
	})
	if err := app.Confirm(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	corr, _ := st.UnprocessedCorrections(ctx)
	if len(corr) != 1 || corr[0].CorrectedAction != domain.ActionNoop {
		t.Errorf("confirm should record the noop sentinel: %+v", corr)
	}
	if len(fake.inputs) != 0 {
		t.Errorf("confirmed noop must never send, sent %v", fake.inputs)
	}
}

func TestResolveNoopSkipsSend(t *testing.T) {
	// An explicit `resolve --action @noop --send` records the correction
	// but skips delivery: "do nothing" means exactly that.
	app, st := testApp(t)
	fake := &fakeHerdr{}
	app.Herdr = fake
	ctx := context.Background()

	id, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w1:p10", SituationType: domain.SituationApproval, Trigger: "t",
		Action: "escalated", Status: "escalated",
		Suggestion: "respond: y", CreatedAt: time.Now(),
	})
	if err := app.Resolve(ctx, id, domain.ActionNoop, true); err != nil {
		t.Fatal(err)
	}
	corr, _ := st.UnprocessedCorrections(ctx)
	if len(corr) != 1 || corr[0].CorrectedAction != domain.ActionNoop {
		t.Errorf("resolve should record the noop correction: %+v", corr)
	}
	if len(fake.inputs) != 0 {
		t.Errorf("noop resolve must never send, sent %v", fake.inputs)
	}
}

func TestResolveNormalizesNoopSpelling(t *testing.T) {
	// The human surface accepts the same spellings as the MCP surface: a
	// bare "noop" is the sentinel, never literal pane text.
	app, st := testApp(t)
	fake := &fakeHerdr{}
	app.Herdr = fake
	ctx := context.Background()

	id, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w1:p11", SituationType: domain.SituationApproval, Trigger: "t",
		Action: "escalated", Status: "escalated",
		Suggestion: "respond: y", CreatedAt: time.Now(),
	})
	if err := app.Resolve(ctx, id, "noop", true); err != nil {
		t.Fatal(err)
	}
	corr, _ := st.UnprocessedCorrections(ctx)
	if len(corr) != 1 || corr[0].CorrectedAction != domain.ActionNoop {
		t.Errorf("bare noop spelling should normalize to the sentinel: %+v", corr)
	}
	if len(fake.inputs) != 0 {
		t.Errorf("normalized noop must never send, sent %v", fake.inputs)
	}
}

// fakeKeyHerdr adds keystroke support (ports.KeystrokeSender) to fakeHerdr.
type fakeKeyHerdr struct {
	fakeHerdr
	keys            []string
	keyScript       []string
	keyScriptFrames []string
	// mcqTabs, when > 0, simulates a live Claude multi-tab form of that many
	// tabs (Submit included) in the PLAIN rendering: a digit commits the
	// current tab's answer (its ☐ becomes ☒) and advances, and the Submit
	// tab's digit submits the form away. Delivery verifies every keystroke
	// against the pane (see internal/mcqdeliver), so a fake whose content never
	// reacts to a digit can not deliver at all.
	mcqTabs      int
	mcqAnswered  int
	mcqSubmitted bool
}

func (f *fakeKeyHerdr) SendKey(_ context.Context, paneID, key string) error {
	f.keys = append(f.keys, key)
	if len(f.keyScript) > 0 && len(f.keyScriptFrames) > 0 && key == f.keyScript[0] {
		f.keyScript = f.keyScript[1:]
		f.pane = f.keyScriptFrames[0]
		f.keyScriptFrames = f.keyScriptFrames[1:]
	}
	if f.mcqTabs > 0 && !f.mcqSubmitted {
		if _, err := strconv.Atoi(key); err == nil {
			if f.mcqAnswered >= f.mcqTabs-1 {
				f.mcqSubmitted = true // the Submit tab's digit
			} else {
				f.mcqAnswered++
			}
		}
	}
	return nil
}

// ReadPane serves the simulated form when one is configured: the header's ☐
// marks reflect how many tabs a digit has answered, and once submitted the
// form is gone.
func (f *fakeKeyHerdr) ReadPane(ctx context.Context, paneID string, lines int) (string, error) {
	if f.mcqTabs == 0 {
		return f.fakeHerdr.ReadPane(ctx, paneID, lines)
	}
	if f.readErr != nil {
		return "", f.readErr
	}
	if f.mcqSubmitted {
		return "⏺ Answers received.\n\n❯ \n", nil
	}
	marks := make([]string, 0, f.mcqTabs-1)
	for i := 0; i < f.mcqTabs-1; i++ {
		if i < f.mcqAnswered {
			marks = append(marks, "☒ Q"+strconv.Itoa(i+1))
		} else {
			marks = append(marks, "☐ Q"+strconv.Itoa(i+1))
		}
	}
	// The question line must change per tab: delivery compares it across a
	// keystroke to prove the form did not silently move to another tab.
	return "←  " + strings.Join(marks, "  ") + "  ✔ Submit  →\n\nQuestion " +
		strconv.Itoa(f.mcqAnswered+1) + "?\n❯ 1. sqlite\n  2. postgres\n\n" +
		"Enter to select · ↑/↓ to navigate · Tab to switch questions · Esc to cancel\n", nil
}

func frontendCodexFrame(current, total, unanswered int, selected string, submitAll bool) string {
	verb := "answer"
	if submitAll {
		verb = "all"
	}
	marker1, marker2 := " ", " "
	if selected == "1" {
		marker1 = "›"
	}
	if selected == "2" {
		marker2 = "›"
	}
	return fmt.Sprintf("Question %d/%d (%d unanswered)\nQuestion %d?\n%s 1. First\n%s 2. Second\n\ntab to add notes | enter to submit %s | ←/→ to navigate questions | esc to interrupt\n",
		current, total, unanswered, current, marker1, marker2, verb)
}

func TestResolveDigitSeriesDeliversKeystrokes(t *testing.T) {
	// Confirming a multi-tab answer series delivers one digit keystroke per
	// tab (Submit included) — never the series as literal text.
	app, st := testApp(t)
	fake := &fakeKeyHerdr{mcqTabs: 3}
	app.Herdr = fake
	ctx := context.Background()

	id, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w1:p12", SituationType: domain.SituationChoice, Trigger: "t",
		Action: "escalated", Status: "escalated",
		Suggestion: "answer series: 1 2 1", CreatedAt: time.Now(),
	})
	if err := app.Confirm(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	corr, _ := st.UnprocessedCorrections(ctx)
	if len(corr) != 1 || corr[0].CorrectedAction != "1 2 1" {
		t.Errorf("confirm should record the series: %+v", corr)
	}
	if len(fake.inputs) != 0 {
		t.Errorf("series must not be sent as text, sent %v", fake.inputs)
	}
	// All-or-nothing delivery: a read-only baseline pre-pass first (reset, then
	// a Right-arrow walk of tabs 2 and 3) confirms the form is stable and no
	// multi-select tab is pre-selected, then the delivery pass resets again and
	// types one digit per (single-select) tab.
	reset := strings.TrimSpace(strings.Repeat("left ", 10))
	want := reset + " right right " + reset + " 1 2 1"
	if got := strings.Join(fake.keys, " "); got != want {
		t.Errorf("keystrokes = %q, want %q", got, want)
	}
}

func TestResolveDigitSeriesStaleFormFails(t *testing.T) {
	// The pane no longer shows a matching multi-tab form: the correction is
	// recorded but no digit is typed into whatever replaced it.
	app, st := testApp(t)
	fake := &fakeKeyHerdr{fakeHerdr: fakeHerdr{pane: "$ waiting at a shell prompt\n"}}
	app.Herdr = fake
	ctx := context.Background()

	id, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w1:p13", SituationType: domain.SituationChoice, Trigger: "t",
		Action: "escalated", Status: "escalated",
		Suggestion: "answer series: 1 2 1", CreatedAt: time.Now(),
	})
	err := app.Resolve(ctx, id, "1 2 1", true)
	if err == nil || !strings.Contains(err.Error(), "no longer shows") {
		t.Fatalf("stale form must fail the send, got err=%v", err)
	}
	if len(fake.keys) != 0 || len(fake.inputs) != 0 {
		t.Errorf("nothing may reach the pane: keys=%v inputs=%v", fake.keys, fake.inputs)
	}
	corr, _ := st.UnprocessedCorrections(ctx)
	if len(corr) != 1 {
		t.Errorf("the correction itself must still be recorded: %+v", corr)
	}
}

func TestResolveCodexQuestionSeriesDeliversAdaptiveKeystrokes(t *testing.T) {
	app, st := testApp(t)
	initial := frontendCodexFrame(1, 2, 2, "1", false)
	selected := frontendCodexFrame(1, 2, 2, "2", false)
	second := frontendCodexFrame(2, 2, 1, "1", false)
	ready := frontendCodexFrame(2, 2, 0, "1", true)
	fake := &fakeKeyHerdr{
		fakeHerdr:       fakeHerdr{pane: initial},
		keyScript:       []string{"2", "enter", "1", "enter"},
		keyScriptFrames: []string{selected, second, ready, "submitted"},
	}
	app.Herdr = fake
	ctx := context.Background()
	id, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w1:codex", AgentType: "codex", SituationType: domain.SituationChoice,
		Trigger: "t", Action: "escalated", Status: "escalated",
		Suggestion: "answer series: 2 1", CreatedAt: time.Now(),
	})
	if err := app.Confirm(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	if len(fake.inputs) != 0 {
		t.Fatalf("Codex answer series must use keys, sent text %v", fake.inputs)
	}
	if got := strings.Join(fake.keys, " "); !strings.HasSuffix(got, "2 enter 1 enter") {
		t.Fatalf("Codex adaptive delivery keys = %q", got)
	}
}

func TestSignatureSnapshotAccessor(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()

	if err := st.SaveSignatureSnapshot(ctx, "approval:feed0000beef1111",
		"Do you want to proceed?\n1. Yes\n2. No", time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := app.SignatureSnapshot(ctx, "approval:feed0000beef1111"); !strings.Contains(got, "proceed") {
		t.Errorf("snapshot hit = %q, want the stored excerpt", got)
	}
	if got := app.SignatureSnapshot(ctx, "approval:unknown0000000000"); got != "" {
		t.Errorf("snapshot miss should be empty, got %q", got)
	}
	if got := app.SignatureSnapshot(ctx, ""); got != "" {
		t.Errorf("empty signature should be empty, got %q", got)
	}
}

func TestMatchSummary(t *testing.T) {
	cases := []struct {
		name string
		rec  domain.AuditRecord
		want string
	}{
		{"cosine names similarity_threshold",
			domain.AuditRecord{MatchMethod: domain.MatchCosine, MatchScore: 0.94},
			"matched by `similarity_threshold` (cosine 0.94)"},
		{"bm25 names bm25_min_score and notes fallback",
			domain.AuditRecord{MatchMethod: domain.MatchBM25, MatchScore: 0.42},
			"matched by `bm25_min_score` (bm25 0.42, embedding fallback)"},
		{"exact", domain.AuditRecord{MatchMethod: domain.MatchExact}, "exact content hash"},
		{"none is empty", domain.AuditRecord{MatchMethod: domain.MatchNone}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := frontend.MatchSummary(c.rec); got != c.want {
				t.Errorf("MatchSummary = %q, want %q", got, c.want)
			}
		})
	}
}

func TestFocusAgent(t *testing.T) {
	app, _ := testApp(t)
	focusHerdr := &focusPortHerdr{}
	app.Herdr = focusHerdr
	ctx := context.Background()

	if err := app.FocusAgent(ctx, "2:3", "2-1"); err != nil {
		t.Fatal(err)
	}
	if len(focusHerdr.calls) != 1 || focusHerdr.calls[0].tabID != "2:3" || focusHerdr.calls[0].paneID != "2-1" {
		t.Errorf("FocusAgent should call FocusPane(2:3, 2-1), got %+v", focusHerdr.calls)
	}
}

func TestFocusAgentNotSupported(t *testing.T) {
	app, _ := testApp(t)
	app.Herdr = &fakeHerdr{}
	err := app.FocusAgent(context.Background(), "1:1", "1-1")
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Errorf("FocusAgent without FocusPort should report not supported, got %v", err)
	}
}

type focusPaneCall struct {
	tabID  string
	paneID string
}

type focusPortHerdr struct {
	fakeHerdr
	calls []focusPaneCall
}

func (h *focusPortHerdr) FocusPane(ctx context.Context, tabID, paneID string) error {
	h.calls = append(h.calls, focusPaneCall{tabID, paneID})
	return nil
}

// TestConcurrentTaskMutationsDoNotLose exercises the per-path lock in
// mutateTaskFile: many concurrent AddTask calls on the same checklist must all
// land, since each read-modify-rename is serialized. Without the lock, two
// mutations reading the same content would have the last rename drop the other.
func TestConcurrentTaskMutationsDoNotLose(t *testing.T) {
	app, _ := testApp(t)
	path := filepath.Join(t.TempDir(), "tasks.md")
	if err := os.WriteFile(path, []byte("- [ ] seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	const n = 20
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, errs[i] = app.AddTask("", path, fmt.Sprintf("task-%d", i))
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent AddTask #%d failed: %v", i, err)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	items := domain.ParseChecklist(string(data))
	if len(items) != n+1 { // seed + n added
		t.Fatalf("after %d concurrent adds got %d items, want %d — a mutation was lost", n, len(items), n+1)
	}
}

// TestMutatePreservesFileMode covers the reviewer's compatibility fix: editing
// a normal 0644 checklist must not narrow it to 0600 on every write.
func TestMutatePreservesFileMode(t *testing.T) {
	app, _ := testApp(t)
	path := filepath.Join(t.TempDir(), "tasks.md")
	if err := os.WriteFile(path, []byte("- [ ] a\n- [ ] b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil { // defeat umask so the baseline is exactly 0644
		t.Fatal(err)
	}

	if _, err := app.SetTaskDone("", path, 1, true); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Errorf("file mode after edit = %o, want preserved 0644 (not narrowed to 0600)", got)
	}
}

// TestTaskGroups covers the aggregated all-sources view (TUI Tasks tab): one
// group per config entry in config order, per-source read failures isolated
// to their own group, duplicate paths read independently.
func TestTaskGroups(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "tasks.md")
	if err := os.WriteFile(good, []byte("# plan\n- [ ] a\n- [x] b\n- [-] c\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "prose.md")
	if err := os.WriteFile(empty, []byte("# notes\nno checklist here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "gone.md")

	cfg := config.Config{TaskSources: []config.TaskSource{
		{Agent: "brave-otter", Workspace: "w1", Path: good},
		{Agent: "codex", Path: missing},
		{Agent: "empty-path"},
		{Workspace: "*", Path: good}, // duplicate path, its own group
		{Agent: "quiet", Path: empty},
	}}
	groups := frontend.TaskGroups(cfg)
	if len(groups) != len(cfg.TaskSources) {
		t.Fatalf("got %d groups, want %d", len(groups), len(cfg.TaskSources))
	}
	for i, g := range groups {
		if g.Index != i || g.Source.Path != cfg.TaskSources[i].Path {
			t.Errorf("group %d: Index=%d Path=%q, want config order preserved", i, g.Index, g.Source.Path)
		}
	}

	if g := groups[0]; g.Err != "" || len(g.Items) != 3 {
		t.Fatalf("readable source: Err=%q items=%d, want no error and 3 items", g.Err, len(g.Items))
	}
	wantItems := []struct {
		mark string
		done bool
		text string
	}{{" ", false, "a"}, {"x", true, "b"}, {"-", true, "c"}}
	for i, want := range wantItems {
		it := groups[0].Items[i]
		if it.Mark != want.mark || it.Done != want.done || it.Text != want.text || it.Index != i+1 {
			t.Errorf("item %d = %+v, want mark=%q done=%v text=%q", i, it, want.mark, want.done, want.text)
		}
	}

	if g := groups[1]; g.Err == "" || len(g.Items) != 0 {
		t.Errorf("missing file: Err=%q items=%d, want an error and no items", g.Err, len(g.Items))
	}
	if g := groups[2]; g.Err != "no path configured" {
		t.Errorf("empty path: Err=%q, want \"no path configured\"", g.Err)
	}
	if g := groups[3]; g.Err != "" || len(g.Items) != 3 {
		t.Errorf("duplicate path: Err=%q items=%d, want an independent readable group", g.Err, len(g.Items))
	}
	if g := groups[4]; g.Err != "" || len(g.Items) != 0 {
		t.Errorf("readable file without checklist items: Err=%q items=%d, want no error and no items", g.Err, len(g.Items))
	}
}

func TestTaskGroupsEmptyConfig(t *testing.T) {
	if groups := frontend.TaskGroups(config.Config{}); len(groups) != 0 {
		t.Errorf("no task sources should yield no groups, got %d", len(groups))
	}
}

// TestTaskMutationsVerifyExpectedText pins the optional expected-text guard:
// a mutation whose caller resolved the task number against a checklist that
// has since changed must abort inside the lock, leaving the file untouched.
func TestTaskMutationsVerifyExpectedText(t *testing.T) {
	app, _ := testApp(t)
	newFile := func() string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "tasks.md")
		if err := os.WriteFile(path, []byte("- [ ] alpha\n- [x] beta\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	cases := []struct {
		name   string
		run    func(path string, expect ...string) error
		expect string // guard value; the file's task #1 is "alpha"
	}{
		{"done", func(p string, e ...string) error {
			_, err := app.SetTaskDone("", p, 1, true, e...)
			return err
		}, "stale"},
		{"edit", func(p string, e ...string) error {
			_, err := app.EditTask("", p, 1, "rewritten", e...)
			return err
		}, "stale"},
		{"delete", func(p string, e ...string) error {
			_, err := app.DeleteTask("", p, 1, e...)
			return err
		}, "stale"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := newFile()
			if err := tc.run(path, tc.expect); err == nil || !strings.Contains(err.Error(), "checklist changed") {
				t.Fatalf("mismatched expected text should abort, got %v", err)
			}
			data, _ := os.ReadFile(path)
			if string(data) != "- [ ] alpha\n- [x] beta\n" {
				t.Errorf("aborted %s must not modify the file, got:\n%s", tc.name, data)
			}
			// The matching text (and the no-guard CLI form) still mutates.
			if err := tc.run(path, "alpha"); err != nil {
				t.Fatalf("matching expected text should pass: %v", err)
			}
			if err := tc.run(newFile()); err != nil {
				t.Fatalf("guard must stay optional for CLI callers: %v", err)
			}
		})
	}
	// An out-of-range number reports "no longer exists".
	path := newFile()
	if _, err := app.DeleteTask("", path, 9, "alpha"); err == nil || !strings.Contains(err.Error(), "no longer exists") {
		t.Errorf("vanished task number should abort with a refresh hint, got %v", err)
	}
}

// TestEditTaskMultiline: line breaks in the new text are stored as literal
// `\n` — the item stays ONE task on one physical line, its status and the
// rest of the file untouched — and the expected-text guard still composes.
func TestEditTaskMultiline(t *testing.T) {
	app, _ := testApp(t)
	path := filepath.Join(t.TempDir(), "tasks.md")
	if err := os.WriteFile(path, []byte("- [x] one\n- [ ] two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	items, err := app.EditTask("", path, 1, "first\nsecond", "one")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("a multi-line edit must not change the item count, got %d: %+v", len(items), items)
	}
	data, _ := os.ReadFile(path)
	if want := "- [x] first\\nsecond\n- [ ] two\n"; string(data) != want {
		t.Errorf("multiline edit should store literal \\n:\ngot  %q\nwant %q", data, want)
	}
	if _, err := app.EditTask("", path, 1, "a\nb", "stale"); err == nil {
		t.Error("guard must still abort a stale multiline edit")
	}
	// Bare-\r line breaks (terminal bracketed paste) encode the same way.
	if _, err := app.EditTask("", path, 2, "cr-a\rcr-b"); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	if !strings.Contains(string(data), `- [ ] cr-a\ncr-b`) {
		t.Errorf("CR paste should encode to literal \\n, got %q", data)
	}
}

// TestAddTaskMultiline: newline-bearing text appends ONE item with the
// breaks stored as literal `\n` (leading/trailing whitespace trimmed).
func TestAddTaskMultiline(t *testing.T) {
	app, _ := testApp(t)
	path := filepath.Join(t.TempDir(), "tasks.md")
	if err := os.WriteFile(path, []byte("- [x] done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	items, n, err := app.AddTask("", path, "one\r\ntwo")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 || len(items) != 2 {
		t.Fatalf("got new index %d and %d items, want 2 and 2", n, len(items))
	}
	data, _ := os.ReadFile(path)
	if want := "- [x] done\n- [ ] one\\ntwo\n"; string(data) != want {
		t.Errorf("multiline add:\ngot  %q\nwant %q", data, want)
	}
	if _, _, err := app.AddTask("", path, " \n \r "); err == nil {
		t.Error("all-blank multiline text must error")
	}
	// A literal backslash-n TYPED in the text is indistinguishable from an
	// encoded break by design: it is stored verbatim and will be delivered
	// as a real newline (the documented ambiguity of the `\n` encoding).
	if _, _, err := app.AddTask("", path, `uses \n escape`); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	if !strings.Contains(string(data), `- [ ] uses \n escape`) {
		t.Errorf("typed literal \\n must be stored verbatim, got %q", data)
	}
}

// TestSendTaskToAgent: the pending task is re-verified against the live file
// (freshness guard), rendered through the source's template — stored `\n`
// decoded to real newlines — and delivered to the agent's pane.
func TestSendTaskToAgent(t *testing.T) {
	app, _ := testApp(t)
	h := &sendCaptureHerdr{agents: idleAt("w1:p2")}
	app.Herdr = h
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tasks.md")
	if err := os.WriteFile(path, []byte(`- [ ] step one\nstep two`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := app.SendTaskToAgent(ctx, "w1:p2", "claude", "brave-otter",
		path, "", 1, `step one\nstep two`); err != nil {
		t.Fatal(err)
	}
	if len(h.sent) != 1 {
		t.Fatalf("expected one delivery, got %v", h.sent)
	}
	for _, want := range []string{"step one\nstep two", "brave-otter", path} {
		if !strings.Contains(h.sent[0], want) {
			t.Errorf("sent prompt missing %q:\n%s", want, h.sent[0])
		}
	}
	// A successful send marks the item [-] in progress.
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), `- [-] step one\nstep two`) {
		t.Errorf("sent task should be marked in progress, got %q", data)
	}

	// Freshness guard: a task completed or rewritten since the snapshot
	// refuses to send instead of re-delivering stale work.
	if err := os.WriteFile(path, []byte(`- [x] step one\nstep two`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := app.SendTaskToAgent(ctx, "w1:p2", "claude", "n", path, "", 1, `step one\nstep two`); err == nil ||
		!strings.Contains(err.Error(), "no longer pending") {
		t.Errorf("completed task must refuse to send, got %v", err)
	}
	if err := app.SendTaskToAgent(ctx, "w1:p2", "claude", "n", path, "", 1, "different text"); err == nil ||
		!strings.Contains(err.Error(), "the checklist changed") {
		t.Errorf("rewritten task must refuse to send, got %v", err)
	}
	if len(h.sent) != 1 {
		t.Errorf("refused sends must not deliver, got %v", h.sent)
	}

	// Guards: no herdr / no pane.
	app.Herdr = nil
	if err := app.SendTaskToAgent(ctx, "w1:p2", "claude", "n", path, "", 1, "t"); err == nil {
		t.Error("nil herdr must refuse")
	}
	app.Herdr = h
	if err := app.SendTaskToAgent(ctx, "", "claude", "n", path, "", 1, "t"); err == nil {
		t.Error("empty pane must refuse")
	}
}

// TestSendTaskToAgentRechecksIdle pins the guard against the window between
// the caller's status read and delivery: the operator's confirmation (or a
// --yes script) can be seconds stale, and a task must never land in a working
// agent's live conversation.
func TestSendTaskToAgentRechecksIdle(t *testing.T) {
	newApp := func(t *testing.T, h *sendCaptureHerdr) (*frontend.App, string) {
		t.Helper()
		app, _ := testApp(t)
		app.Herdr = h
		path := filepath.Join(t.TempDir(), "tasks.md")
		if err := os.WriteFile(path, []byte("- [ ] work\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return app, path
	}
	ctx := context.Background()
	// The agent started working after the caller looked.
	busy := &sendCaptureHerdr{agents: []domain.AgentTransition{
		{AgentID: "w1:p2", PaneID: "w1:p2", AgentType: "claude", Status: "working"}}}
	app, path := newApp(t, busy)
	if err := app.SendTaskToAgent(ctx, "w1:p2", "claude", "otter", path, "", 1, "work"); err == nil ||
		!strings.Contains(err.Error(), "cleanly idle") {
		t.Errorf("a now-busy agent must refuse, got %v", err)
	}
	if len(busy.sent) != 0 {
		t.Errorf("refused send must not deliver, got %v", busy.sent)
	}
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), "- [ ] work") {
		t.Errorf("refused send must leave the task pending, got %q", data)
	}
	// The agent vanished entirely.
	gone := &sendCaptureHerdr{}
	app, path = newApp(t, gone)
	if err := app.SendTaskToAgent(ctx, "w1:p2", "claude", "otter", path, "", 1, "work"); err == nil ||
		!strings.Contains(err.Error(), "no longer live") {
		t.Errorf("a vanished agent must refuse, got %v", err)
	}
	// An unreadable agent list is not an idle agent: fail closed.
	app, path = newApp(t, nil)
	app.Herdr = &failingAgentsHerdr{}
	if err := app.SendTaskToAgent(ctx, "w1:p2", "claude", "otter", path, "", 1, "work"); err == nil ||
		!strings.Contains(err.Error(), "nothing was sent") {
		t.Errorf("an unreadable agent list must refuse, got %v", err)
	}
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), "- [ ] work") {
		t.Errorf("refused send must leave the task pending, got %q", data)
	}
}

// TestSendTaskToAgentReservesBeforeDelivering pins the ordering: the item is
// marked [-] BEFORE the pane receives it, so no guarded failure can be
// reported after delivery and leave the task [ ] for the daemon to hand out a
// second time. A failed delivery rolls the reservation back.
func TestSendTaskToAgentReservesBeforeDelivering(t *testing.T) {
	ctx := context.Background()
	app, _ := testApp(t)
	path := filepath.Join(t.TempDir(), "tasks.md")
	if err := os.WriteFile(path, []byte("- [ ] work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The reservation must already be on disk by the time Send is called.
	var atSend string
	h := &sendCaptureHerdr{agents: idleAt("w1:p2")}
	app.Herdr = &reserveProbeHerdr{sendCaptureHerdr: h, path: path, seen: &atSend}
	if err := app.SendTaskToAgent(ctx, "w1:p2", "claude", "otter", path, "", 1, "work"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(atSend, "- [-] work") {
		t.Errorf("task must be reserved [-] BEFORE delivery, file at send time was %q", atSend)
	}
	// A delivery that fails returns the task to [ ] rather than parking it.
	app2, _ := testApp(t)
	path2 := filepath.Join(t.TempDir(), "tasks.md")
	if err := os.WriteFile(path2, []byte("- [ ] work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	app2.Herdr = &sendCaptureHerdr{agents: idleAt("w1:p2"), sendErr: errors.New("pane gone")}
	if err := app2.SendTaskToAgent(ctx, "w1:p2", "claude", "otter", path2, "", 1, "work"); err == nil ||
		!strings.Contains(err.Error(), "pane gone") {
		t.Errorf("a failed delivery must surface its error, got %v", err)
	}
	if data, _ := os.ReadFile(path2); !strings.Contains(string(data), "- [ ] work") {
		t.Errorf("a failed delivery must roll the reservation back to [ ], got %q", data)
	}
}

// reserveProbeHerdr snapshots the checklist file at the moment of delivery.
type reserveProbeHerdr struct {
	*sendCaptureHerdr
	path string
	seen *string
}

func (c *reserveProbeHerdr) Send(ctx context.Context, pane, input string) error {
	data, _ := os.ReadFile(c.path)
	*c.seen = string(data)
	return c.sendCaptureHerdr.Send(ctx, pane, input)
}

// racingHerdr rewrites the checklist during the delivery — standing in for
// another operator acting inside the send's lock-release window — and then
// fails the send, forcing the rollback to confront the change.
type racingHerdr struct {
	sendCaptureHerdr
	path, write string
}

func (c *racingHerdr) Send(context.Context, string, string) error {
	_ = os.WriteFile(c.path, []byte(c.write), 0o644)
	return errors.New("pane gone")
}

// TestSendTaskToAgentRollbackIsClaimScoped: the rollback only reopens an item
// that is still the [-] this send reserved. Someone else's completion landing
// in the window must survive — reopening it would both discard their work and
// re-arm the task for the daemon.
func TestSendTaskToAgentRollbackIsClaimScoped(t *testing.T) {
	ctx := context.Background()
	app, _ := testApp(t)
	path := filepath.Join(t.TempDir(), "tasks.md")
	if err := os.WriteFile(path, []byte("- [ ] work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	app.Herdr = &racingHerdr{
		sendCaptureHerdr: sendCaptureHerdr{agents: idleAt("w1:p2")},
		path:             path,
		write:            "- [x] work\n", // completed by someone else mid-send
	}
	err := app.SendTaskToAgent(ctx, "w1:p2", "claude", "otter", path, "", 1, "work")
	if err == nil || !strings.Contains(err.Error(), "pane gone") {
		t.Errorf("the delivery failure must still surface, got %v", err)
	}
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), "- [x] work") {
		t.Errorf("a concurrent completion must not be reopened by the rollback, got %q", data)
	}
}

// TestSendTaskToAgentRendersCwd pins that a manual send fills {cwd} the same
// way the daemon's declared-task path does — one template must not render
// differently depending on who sent it.
func TestSendTaskToAgentRendersCwd(t *testing.T) {
	ctx := context.Background()
	app, _ := testApp(t)
	path := filepath.Join(t.TempDir(), "tasks.md")
	if err := os.WriteFile(path, []byte("- [ ] work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := &sendInspectHerdr{
		sendCaptureHerdr: sendCaptureHerdr{agents: idleAt("w1:p2")},
		info:             domain.PaneInfo{Cwd: "/repo", ForegroundCwd: "/repo/sub"},
	}
	app.Herdr = h
	if err := app.SendTaskToAgent(ctx, "w1:p2", "claude", "otter", path,
		"do {next_task_content} in {cwd}", 1, "work"); err != nil {
		t.Fatal(err)
	}
	// The foreground cwd wins, exactly as the daemon's resolver prefers it.
	if len(h.sent) != 1 || !strings.Contains(h.sent[0], "do work in /repo/sub") {
		t.Errorf("{cwd} should render the foreground cwd, got %v", h.sent)
	}
	// An adapter without the optional inspector still sends, with {cwd} empty.
	app2, _ := testApp(t)
	plain := &sendCaptureHerdr{agents: idleAt("w1:p2")}
	app2.Herdr = plain
	path2 := filepath.Join(t.TempDir(), "tasks.md")
	if err := os.WriteFile(path2, []byte("- [ ] work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := app2.SendTaskToAgent(ctx, "w1:p2", "claude", "otter", path2,
		"do {next_task_content} in {cwd}", 1, "work"); err != nil {
		t.Errorf("a missing inspector must never block a send, got %v", err)
	}
	// Exactly, not Contains: "do work in " is a prefix of a resolved cwd too,
	// so a substring check could not tell an empty {cwd} from a filled one.
	if len(plain.sent) != 1 || plain.sent[0] != "do work in " {
		t.Errorf("expected a delivery with an empty cwd, got %q", plain.sent)
	}
}

// sendCaptureHerdr records deliveries and reports the agents it is given, so
// SendTaskToAgent's just-before-delivery idle re-check can resolve the pane.
// sendErr makes the delivery itself fail (the rollback path).
type sendCaptureHerdr struct {
	sent    []string
	agents  []domain.AgentTransition
	sendErr error
}

func (c *sendCaptureHerdr) Send(_ context.Context, _, input string) error {
	if c.sendErr != nil {
		return c.sendErr
	}
	c.sent = append(c.sent, input)
	return nil
}
func (c *sendCaptureHerdr) ReadPane(context.Context, string, int) (string, error) { return "", nil }
func (c *sendCaptureHerdr) ListAgents(context.Context) ([]domain.AgentTransition, error) {
	return c.agents, nil
}

// idleAt builds the one-agent listing the send path expects.
func idleAt(paneID string) []domain.AgentTransition {
	return []domain.AgentTransition{{AgentID: paneID, PaneID: paneID, AgentType: "claude", Status: "idle"}}
}

// sendInspectHerdr adds the optional InspectorPort so {cwd} can resolve.
type sendInspectHerdr struct {
	sendCaptureHerdr
	info domain.PaneInfo
}

func (c *sendInspectHerdr) PaneInfo(context.Context, string) (domain.PaneInfo, error) {
	return c.info, nil
}

// TestAddTaskRespectsMaxTasksCap: a manual add to a registered source is
// rejected once it would push the checklist past the source's max_tasks cap,
// while an ad-hoc --path file (no registered source) is uncapped.
func TestAddTaskRespectsMaxTasksCap(t *testing.T) {
	app, _ := testApp(t)
	dir := filepath.Dir(app.ConfigPath)
	taskFile := filepath.Join(dir, "tasks.md")
	if err := os.WriteFile(taskFile, []byte("- [ ] one\n- [x] two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Register the file as a source capped at 3.
	cfgToml := fmt.Sprintf("[[task_sources]]\nagent = \"builder\"\npath = %q\nmax_tasks = 3\n", taskFile)
	if err := os.WriteFile(app.ConfigPath, []byte(cfgToml), 0o644); err != nil {
		t.Fatal(err)
	}

	// 2 items → adding one more reaches the cap (3), still allowed.
	if _, _, err := app.AddTask("builder", "", "three"); err != nil {
		t.Fatalf("adding up to the cap must succeed: %v", err)
	}
	// 3 items → the next add would be 4 > 3, rejected with the cap message.
	_, _, err := app.AddTask("builder", "", "four")
	if err == nil || !strings.Contains(err.Error(), "maximum number of tasks reached") {
		t.Fatalf("adding past the cap must be rejected with the cap message, got %v", err)
	}

	// A line-break-bearing add stays ONE task (stored with literal `\n`), so
	// it counts once against the cap: 2 items + 1 multi-line task = 3 ≤ cap.
	if err := os.WriteFile(taskFile, []byte("- [ ] a\n- [x] b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := app.AddTask("builder", "", "c1\nc2\nc3"); err != nil {
		t.Fatalf("a multi-line add is one task and must fit the cap: %v", err)
	}
	if data, _ := os.ReadFile(taskFile); len(domain.ParseChecklist(string(data))) != 3 {
		t.Errorf("multi-line text must store as a single item, got %q", data)
	}

	// An unregistered --path file has no source entry and is uncapped.
	adhoc := filepath.Join(dir, "adhoc.md")
	if err := os.WriteFile(adhoc, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, _, err := app.AddTask("", adhoc, fmt.Sprintf("t%d", i)); err != nil {
			t.Fatalf("an unregistered --path file must be uncapped; add %d failed: %v", i, err)
		}
	}
}

// TestPendingTasks: only unchecked ("[ ]") items count, and unreadable
// sources are skipped (their contents are unknown, not zero).
func TestPendingTasks(t *testing.T) {
	groups := []frontend.TaskGroup{
		{Items: []domain.ChecklistItem{{Mark: " "}, {Mark: "x", Done: true}, {Mark: " "}}},
		{Err: "open: no such file", Items: []domain.ChecklistItem{{Mark: " "}}},
		{Items: []domain.ChecklistItem{{Mark: "-", Done: true}}},
	}
	if got := frontend.PendingTasks(groups); got != 2 {
		t.Errorf("PendingTasks = %d, want 2 (errored group skipped, done/in-progress not pending)", got)
	}
}

func TestUnfinishedTasks(t *testing.T) {
	for _, tc := range []struct {
		name  string
		items []domain.ChecklistItem
		want  int
	}{
		{"pending counts", []domain.ChecklistItem{{Mark: " "}, {Mark: " "}}, 2},
		{"in progress counts", []domain.ChecklistItem{{Mark: "-", Done: true}}, 1},
		{"completed marks do not", []domain.ChecklistItem{
			{Mark: "x", Done: true}, {Mark: "X", Done: true},
			{Mark: "+", Done: true}, {Mark: "*", Done: true}}, 0},
		{"mixed", []domain.ChecklistItem{
			{Mark: " "}, {Mark: "-", Done: true}, {Mark: "x", Done: true}}, 2},
		{"empty list", nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := frontend.UnfinishedTasks([]frontend.TaskGroup{{Items: tc.items}})
			if got != tc.want {
				t.Errorf("UnfinishedTasks = %d, want %d", got, tc.want)
			}
		})
	}
	// Unreadable sources are unknown, not zero — same rule as PendingTasks.
	errored := []frontend.TaskGroup{{Err: "open: no such file", Items: []domain.ChecklistItem{{Mark: " "}}}}
	if got := frontend.UnfinishedTasks(errored); got != 0 {
		t.Errorf("UnfinishedTasks(errored) = %d, want 0 (skipped)", got)
	}
	// The reason this function exists: an agent mid-task leaves "[-]" items,
	// which Done (a pending/not-pending flag) reports as finished. A caller
	// asking "is this list done?" must not use PendingTasks.
	working := []frontend.TaskGroup{{Items: []domain.ChecklistItem{{Mark: "-", Done: true}}}}
	if p, u := frontend.PendingTasks(working), frontend.UnfinishedTasks(working); p != 0 || u != 1 {
		t.Errorf("in-progress list: PendingTasks = %d (want 0), UnfinishedTasks = %d (want 1)", p, u)
	}
}

// TestStatusAgentsKnown pins the distinction callers act on: a failed agent
// query and a genuinely empty herd both leave MonitoredAgents empty, so
// GetStatus must say which one happened. Anything deciding on an agent's
// ABSENCE (the Tasks tab's source removal) is unsafe without it.
func TestStatusAgentsKnown(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		herdr ports.HerdrPort
		want  bool
	}{
		{"query failed", &failingAgentsHerdr{}, false},
		{"no adapter", nil, false},
		{"empty herd", &emptyAgentsHerdr{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, _ := testApp(t)
			app.Herdr = tc.herdr
			st, err := app.GetStatus(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if st.AgentsKnown != tc.want {
				t.Errorf("AgentsKnown = %v, want %v", st.AgentsKnown, tc.want)
			}
			if len(st.MonitoredAgents) != 0 {
				t.Errorf("all cases report zero agents, got %d", len(st.MonitoredAgents))
			}
		})
	}
}

type failingAgentsHerdr struct{}

func (f *failingAgentsHerdr) Send(context.Context, string, string) error { return nil }
func (f *failingAgentsHerdr) ReadPane(context.Context, string, int) (string, error) {
	return "", nil
}
func (f *failingAgentsHerdr) ListAgents(context.Context) ([]domain.AgentTransition, error) {
	return nil, errors.New("herdr unreachable")
}

type emptyAgentsHerdr struct{}

func (e *emptyAgentsHerdr) Send(context.Context, string, string) error { return nil }
func (e *emptyAgentsHerdr) ReadPane(context.Context, string, int) (string, error) {
	return "", nil
}
func (e *emptyAgentsHerdr) ListAgents(context.Context) ([]domain.AgentTransition, error) {
	return nil, nil
}

// TestAddTaskSourceAutoSendWhenIdleOption pins the option that turns on
// unprompted hand-out. Unprompted sending is a safety-relevant capability, so
// it must be reachable ONLY by asking for it: no option, no flag — including
// on the bootstrap path that registers a generated task list by itself, which
// no operator ever opted in for.
func TestAddTaskSourceAutoSendWhenIdleOption(t *testing.T) {
	app, st := testApp(t)
	app.Herdr = &fakeHerdr{}
	app.StateDir = t.TempDir()
	ctx := context.Background()
	dir := t.TempDir()
	plain := filepath.Join(dir, "plain.md")
	auto := filepath.Join(dir, "auto.md")

	if err := app.AddTaskSource(ctx, "quiet-fox", "", plain, ""); err != nil {
		t.Fatal(err)
	}
	if err := app.AddTaskSource(ctx, "busy-otter", "", auto, "", frontend.AutoSendWhenIdle()); err != nil {
		t.Fatal(err)
	}
	cfg, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.TaskSources) != 2 {
		t.Fatalf("want 2 task sources, got %d", len(cfg.TaskSources))
	}
	if cfg.TaskSources[0].EnableAutoSendTaskWhenIdle {
		t.Error("an add with no option must leave auto-send off")
	}
	if !cfg.TaskSources[1].EnableAutoSendTaskWhenIdle {
		t.Error("AutoSendWhenIdle() did not reach the saved source")
	}
	// The option must survive a save/load round trip, not just the in-memory
	// config: the daemon reads the file.
	reloaded, err := config.Load(app.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.TaskSources[0].EnableAutoSendTaskWhenIdle || !reloaded.TaskSources[1].EnableAutoSendTaskWhenIdle {
		t.Errorf("auto-send flags did not round-trip through config.toml: %+v", reloaded.TaskSources)
	}

	// Confirming a generated task list bootstraps a source by itself — no
	// operator ever opted that one in, so it must come out off.
	if _, err := st.EnsureAgentName(ctx, "w9:p9"); err != nil {
		t.Fatal(err)
	}
	id := generatedEscalation(t, st, "w9:p9", "Bootstrap a list")
	if err := app.Confirm(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	if cfg, err = app.Config(); err != nil {
		t.Fatal(err)
	}
	if len(cfg.TaskSources) != 3 {
		t.Fatalf("confirm should have bootstrapped a third source, got %d", len(cfg.TaskSources))
	}
	if cfg.TaskSources[2].EnableAutoSendTaskWhenIdle {
		t.Error("a bootstrapped task source must never enable unprompted hand-out")
	}
	// It still names its cap: a source hap creates itself must not land on disk
	// with "max_tasks" missing, which reads as "no limit".
	if cfg.TaskSources[2].MaxTasks != config.DefaultMaxTasks {
		t.Errorf("bootstrapped source should carry max_tasks=%d, got %d", config.DefaultMaxTasks, cfg.TaskSources[2].MaxTasks)
	}
}

// TestRemoveTaskSourceKeepsChecklistFile pins the contract the TUI's Tasks-tab
// `x` advertises: removing a source retires the config entry only. Source
// files are often hand-written docs hap never created and could not restore.
func TestRemoveTaskSourceKeepsChecklistFile(t *testing.T) {
	app, _ := testApp(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tasks.md")
	if err := os.WriteFile(path, []byte("- [ ] keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := app.AddTaskSource(ctx, "a1", "", path, ""); err != nil {
		t.Fatal(err)
	}
	cfg, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.TaskSources) != 1 {
		t.Fatalf("want 1 task source, got %d", len(cfg.TaskSources))
	}
	if err := app.RemoveTaskSource(ctx, 0, cfg.TaskSources[0]); err != nil {
		t.Fatal(err)
	}
	if cfg, err = app.Config(); err != nil {
		t.Fatal(err)
	}
	if len(cfg.TaskSources) != 0 {
		t.Errorf("entry should be gone, got %d source(s)", len(cfg.TaskSources))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("checklist file must survive removal: %v", err)
	}
	if string(data) != "- [ ] keep me\n" {
		t.Errorf("checklist file must be untouched, got %q", data)
	}
}

// --- Claude "Select remote environment" picker ---

const frontRemoteEnvPane = `   Select remote environment

   Configure environments at: https://claude.ai/code

   ❯ 1. herdr-auto-pilot (env_01F41H1jxkGrT2zj55CqE4WQ) ✔
     2. myspec-monorepo (env_01CASfztpZp7mYRJPK41sGvK)
     3. Full-access (env_011CUW5BKtc4vkq5q1uSp7MY)
     4. Default (env_011CUKn5Aj1q6ujg5PFvEhTE)

   Enter to select · Esc to cancel
`

const frontRemoteEnvPaneCaret4 = `   Select remote environment

   Configure environments at: https://claude.ai/code

     1. herdr-auto-pilot (env_01F41H1jxkGrT2zj55CqE4WQ) ✔
     2. myspec-monorepo (env_01CASfztpZp7mYRJPK41sGvK)
     3. Full-access (env_011CUW5BKtc4vkq5q1uSp7MY)
   ❯ 4. Default (env_011CUKn5Aj1q6ujg5PFvEhTE)

   Enter to select · Esc to cancel
`

// Confirming a remote-environment escalation on a keystroke-capable adapter
// must answer the standing picker adaptively (digit → verify → Enter), never
// a text send, and mark the correction sent only after the picker commits.
func TestConfirmDeliversRemoteEnvSelectionAdaptively(t *testing.T) {
	app, st := testApp(t)
	fake := &fakeKeyHerdr{
		fakeHerdr:       fakeHerdr{pane: frontRemoteEnvPane},
		keyScript:       []string{"4", "enter"},
		keyScriptFrames: []string{frontRemoteEnvPaneCaret4, "● Launching remote agent…\n"},
	}
	app.Herdr = fake
	ctx := context.Background()

	id, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w1:p1", AgentType: "claude", SituationType: domain.SituationApproval,
		Trigger: "t", Action: "escalated", Status: "escalated",
		Suggestion: "LLM suggested: Default (env_011CUKn5Aj1q6ujg5PFvEhTE)", CreatedAt: time.Now(),
	})
	if err := app.Confirm(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fake.keys, ","); got != "4,enter" {
		t.Errorf("keys = %q, want \"4,enter\"", got)
	}
	if len(fake.inputs) != 0 {
		t.Errorf("the picker must be answered with keystrokes, not a text send: %v", fake.inputs)
	}
	corr, _ := st.UnprocessedCorrections(ctx)
	if len(corr) != 1 || !corr[0].Sent {
		t.Errorf("correction should be recorded and marked sent: %+v", corr)
	}
}

// A failed adaptive delivery (the picker refuses the label) keeps the
// correction recorded but NOT sent, and surfaces the error to the operator.
func TestConfirmRemoteEnvUnknownLabelKeepsCorrectionUnsent(t *testing.T) {
	app, st := testApp(t)
	fake := &fakeKeyHerdr{fakeHerdr: fakeHerdr{pane: frontRemoteEnvPane}}
	app.Herdr = fake
	ctx := context.Background()

	id, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w1:p1", AgentType: "claude", SituationType: domain.SituationApproval,
		Trigger: "t", Action: "escalated", Status: "escalated",
		Suggestion: "LLM suggested: other-project (env_01ZZZZZZZZZZZZZZZZZZZZZZZZ)", CreatedAt: time.Now(),
	})
	err := app.Confirm(ctx, id, true)
	if err == nil || !strings.Contains(err.Error(), "correction recorded") {
		t.Fatalf("err = %v, want a correction-recorded delivery failure", err)
	}
	if len(fake.keys) != 0 {
		t.Errorf("no keystroke may be sent for an unmappable label, got %v", fake.keys)
	}
	corr, _ := st.UnprocessedCorrections(ctx)
	if len(corr) != 1 || corr[0].Sent {
		t.Errorf("correction should be recorded but unsent: %+v", corr)
	}
}

// A keystroke-less adapter must fall back to the plain digit text send rather
// than failing the operator's explicit confirm.
func TestConfirmRemoteEnvKeystrokelessFallsBackToTextSend(t *testing.T) {
	app, st := testApp(t)
	fake := &fakeHerdr{pane: frontRemoteEnvPane}
	app.Herdr = fake
	ctx := context.Background()

	id, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w1:p1", AgentType: "claude", SituationType: domain.SituationApproval,
		Trigger: "t", Action: "escalated", Status: "escalated",
		Suggestion: "LLM suggested: Default (env_011CUKn5Aj1q6ujg5PFvEhTE)", CreatedAt: time.Now(),
	})
	if err := app.Confirm(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	if len(fake.inputs) != 1 || fake.inputs[0] != "4" {
		t.Errorf("delivered %v, want the mapped menu digit [\"4\"]", fake.inputs)
	}
}

// A keystroke-less adapter with an UNMAPPABLE label must fail closed even on
// an operator's explicit confirm: the plain text send would type the literal
// label + Enter, and under the caret binding Enter commits whatever option
// the caret rests on. Only a mappable label may fall through to a digit send.
func TestConfirmRemoteEnvKeystrokelessUnknownLabelFailsClosed(t *testing.T) {
	app, st := testApp(t)
	fake := &fakeHerdr{pane: frontRemoteEnvPane}
	app.Herdr = fake
	ctx := context.Background()

	id, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w1:p1", AgentType: "claude", SituationType: domain.SituationApproval,
		Trigger: "t", Action: "escalated", Status: "escalated",
		Suggestion: "LLM suggested: other-project (env_01ZZZZZZZZZZZZZZZZZZZZZZZZ)", CreatedAt: time.Now(),
	})
	err := app.Confirm(ctx, id, true)
	if err == nil || !strings.Contains(err.Error(), "none of the offered environments") {
		t.Fatalf("err = %v, want an unmappable-label refusal", err)
	}
	if len(fake.inputs) != 0 {
		t.Errorf("nothing may be sent for an unmappable label, sent %v", fake.inputs)
	}
	corr, _ := st.UnprocessedCorrections(ctx)
	if len(corr) != 1 || corr[0].Sent {
		t.Errorf("correction should be recorded but unsent: %+v", corr)
	}
}

// A read failure on a remote-environment approval must REFUSE, never fall
// through to the literal-label text send — its trailing Enter could commit
// whatever option the caret rests on. The situation is identified from the
// audit's own pane capture, so the refusal works exactly when the live pane
// is unreadable.
func TestConfirmRemoteEnvReadFailureFailsClosed(t *testing.T) {
	app, st := testApp(t)
	fake := &fakeKeyHerdr{fakeHerdr: fakeHerdr{readErr: errAny}}
	app.Herdr = fake
	ctx := context.Background()

	id, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w1:p1", AgentType: "claude", SituationType: domain.SituationApproval,
		Trigger: "t", Action: "escalated", Status: "escalated",
		Suggestion:  "LLM suggested: Default (env_011CUKn5Aj1q6ujg5PFvEhTE)",
		PaneExcerpt: frontRemoteEnvPane, CreatedAt: time.Now(),
	})
	err := app.Confirm(ctx, id, true)
	if err == nil || !strings.Contains(err.Error(), "could not be read") {
		t.Fatalf("err = %v, want a read-failure refusal", err)
	}
	if len(fake.inputs) != 0 || len(fake.keys) != 0 {
		t.Errorf("nothing may be sent when the pane is unreadable: inputs=%v keys=%v", fake.inputs, fake.keys)
	}
	corr, _ := st.UnprocessedCorrections(ctx)
	if len(corr) != 1 || corr[0].Sent {
		t.Errorf("correction should be recorded but unsent: %+v", corr)
	}
}

// The picker no longer standing (already answered, or the pane moved on) must
// refuse too — identified via the audit capture, so the literal label is
// never typed into whatever replaced the picker.
func TestConfirmRemoteEnvGonePickerFailsClosed(t *testing.T) {
	app, st := testApp(t)
	fake := &fakeKeyHerdr{fakeHerdr: fakeHerdr{pane: "● Environment selected. Launching remote agent…\n"}}
	app.Herdr = fake
	ctx := context.Background()

	id, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w1:p1", AgentType: "claude", SituationType: domain.SituationApproval,
		Trigger: "t", Action: "escalated", Status: "escalated",
		Suggestion:  "LLM suggested: Default (env_011CUKn5Aj1q6ujg5PFvEhTE)",
		PaneExcerpt: frontRemoteEnvPane, CreatedAt: time.Now(),
	})
	err := app.Confirm(ctx, id, true)
	if err == nil || !strings.Contains(err.Error(), "no longer shows") {
		t.Fatalf("err = %v, want a picker-gone refusal", err)
	}
	if len(fake.inputs) != 0 || len(fake.keys) != 0 {
		t.Errorf("nothing may be sent when the picker is gone: inputs=%v keys=%v", fake.inputs, fake.keys)
	}
}

// TestSetTaskSourceSettings covers editing an EXISTING source's two mutable
// settings: the values must reach config.toml (the daemon reads the file), the
// stale-listing guard must refuse an index whose path has moved, and max_tasks
// must refuse 0 — on disk 0 means "unset", so accepting it would silently mean
// the default rather than the "no cap" an operator typing 0 expects.
func TestSetTaskSourceSettings(t *testing.T) {
	app, _ := testApp(t)
	ctx := context.Background()
	dir := t.TempDir()
	first := filepath.Join(dir, "first.md")
	second := filepath.Join(dir, "second.md")
	if err := app.AddTaskSource(ctx, "a1", "", first, ""); err != nil {
		t.Fatal(err)
	}
	if err := app.AddTaskSource(ctx, "a2", "", second, ""); err != nil {
		t.Fatal(err)
	}
	cfg, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	// A new source names its cap explicitly rather than leaving a bare 0.
	if cfg.TaskSources[0].MaxTasks != config.DefaultMaxTasks {
		t.Errorf("a new source should carry max_tasks=%d, got %d", config.DefaultMaxTasks, cfg.TaskSources[0].MaxTasks)
	}
	// The cap can also be chosen at creation time, and is validated there —
	// every surface that offers it inherits this one rule.
	third := filepath.Join(dir, "third.md")
	if err := app.AddTaskSource(ctx, "a3", "", third, "", frontend.MaxTasks(40)); err != nil {
		t.Fatal(err)
	}
	if err := app.AddTaskSource(ctx, "a4", "", filepath.Join(dir, "bad.md"), "", frontend.MaxTasks(0)); err == nil {
		t.Error("MaxTasks(0) must be refused — on disk 0 means unset")
	}
	if cfg, err = app.Config(); err != nil {
		t.Fatal(err)
	}
	if len(cfg.TaskSources) != 3 || cfg.TaskSources[2].MaxTasks != 40 {
		t.Fatalf("MaxTasks option did not persist (and a refused add must register nothing): %+v", cfg.TaskSources)
	}

	if err := app.SetTaskSourceAutoSend(ctx, 0, cfg.TaskSources[0], true); err != nil {
		t.Fatal(err)
	}
	if err := app.SetTaskSourceMaxTasks(ctx, 0, cfg.TaskSources[0], 7); err != nil {
		t.Fatal(err)
	}
	reloaded, err := config.Load(app.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.TaskSources[0].EnableAutoSendTaskWhenIdle || reloaded.TaskSources[0].MaxTasks != 7 {
		t.Errorf("settings did not round-trip through config.toml: %+v", reloaded.TaskSources[0])
	}
	// The other source is untouched — an edit targets exactly one entry.
	if reloaded.TaskSources[1].EnableAutoSendTaskWhenIdle || reloaded.TaskSources[1].MaxTasks != config.DefaultMaxTasks {
		t.Errorf("source #1 must be untouched, got %+v", reloaded.TaskSources[1])
	}

	// Turning it back off must be readable as off after a reload (the key is
	// omitempty, so "off" is an absent key — that is what false means on disk).
	if err := app.SetTaskSourceAutoSend(ctx, 0, cfg.TaskSources[0], false); err != nil {
		t.Fatal(err)
	}
	if reloaded, err = config.Load(app.ConfigPath); err != nil {
		t.Fatal(err)
	}
	if reloaded.TaskSources[0].EnableAutoSendTaskWhenIdle {
		t.Error("auto-send should be off again")
	}

	// Guards.
	if err := app.SetTaskSourceMaxTasks(ctx, 0, cfg.TaskSources[0], 0); err == nil {
		t.Error("max_tasks 0 must be refused — on disk it means unset")
	}
	if err := app.SetTaskSourceMaxTasks(ctx, 0, config.TaskSource{Agent: "a1", Path: "/some/other.md"}, 5); err == nil {
		t.Error("a stale expected path must be refused")
	}
	if err := app.SetTaskSourceAutoSend(ctx, 9, cfg.TaskSources[0], true); err == nil {
		t.Error("an out-of-range index must be refused")
	}
	// Two sources may share a checklist with different scopes, so the guard
	// compares the selectors too — a path-only check would edit the wrong one.
	sameFile := cfg.TaskSources[0]
	sameFile.Agent = "someone-else"
	if err := app.SetTaskSourceMaxTasks(ctx, 0, sameFile, 5); err == nil {
		t.Error("a source whose selectors no longer match must be refused")
	}
}

// TestRemoveTaskSourceDuplicatePathReordered is the regression the path-only
// guard could not catch: two sources may point at the SAME checklist under
// different agent selectors, so after a reorder the index the operator listed
// holds a different entry whose path still matches. Removal must refuse rather
// than retire the wrong agent's source — and the entry the operator DID name
// is still removable by its new index.
func TestRemoveTaskSourceDuplicatePathReordered(t *testing.T) {
	app, _ := testApp(t)
	ctx := context.Background()
	shared := filepath.Join(t.TempDir(), "shared.md")
	if err := os.WriteFile(shared, []byte("- [ ] a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := app.AddTaskSource(ctx, "alpha", "", shared, ""); err != nil {
		t.Fatal(err)
	}
	if err := app.AddTaskSource(ctx, "beta", "", shared, ""); err != nil {
		t.Fatal(err)
	}
	cfg, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	listed := cfg.TaskSources[0] // agent "alpha", at index 0 when listed
	if listed.Agent != "alpha" {
		t.Fatalf("fixture: expected alpha first, got %+v", cfg.TaskSources)
	}

	// Somebody reorders the file underneath (both entries still share a path).
	cfg.TaskSources[0], cfg.TaskSources[1] = cfg.TaskSources[1], cfg.TaskSources[0]
	if err := config.Save(app.ConfigPath, cfg); err != nil {
		t.Fatal(err)
	}
	if err := app.RemoveTaskSource(ctx, 0, listed); err == nil {
		t.Fatal("a reordered duplicate-path entry must refuse removal")
	}
	if cfg, err = app.Config(); err != nil {
		t.Fatal(err)
	}
	if len(cfg.TaskSources) != 2 {
		t.Fatalf("a refused removal must change nothing, got %+v", cfg.TaskSources)
	}

	// Re-listed, the same source removes cleanly from its new index — the
	// guard refuses a stale reference, it does not strand the entry.
	if err := app.RemoveTaskSource(ctx, 1, cfg.TaskSources[1]); err != nil {
		t.Fatal(err)
	}
	if cfg, err = app.Config(); err != nil {
		t.Fatal(err)
	}
	if len(cfg.TaskSources) != 1 || cfg.TaskSources[0].Agent != "beta" {
		t.Errorf("wrong source removed: %+v", cfg.TaskSources)
	}
}

// TestConfirmTaskGenNoopLearnsNoopWithoutTaskSource: a generate-task decline
// escalates as the human-readable noop suggestion, NOT as a generated task.
// Confirming it must learn a plain @noop rule and touch nothing else — no
// tasks.md, no registered task source, and nothing typed into the pane, even
// with --send.
func TestConfirmTaskGenNoopLearnsNoopWithoutTaskSource(t *testing.T) {
	app, st := testApp(t)
	fake := &fakeHerdr{}
	app.Herdr = fake
	stateDir := t.TempDir()
	app.StateDir = stateDir
	ctx := context.Background()

	name, _ := st.EnsureAgentName(ctx, "w9:p9")
	id, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w9:p9", SituationType: domain.SituationIdle, Trigger: "t",
		Signature: "sig-noop", AgentType: "claude",
		Action: "escalated", Status: "escalated",
		Suggestion: domain.ActionNoopSuggestion, CreatedAt: time.Now(),
	})

	// The escalation resolves to the sentinel, never to a generated task.
	audit, err := st.GetAudit(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got := frontend.SuggestedAction(audit); got != domain.ActionNoop {
		t.Fatalf("SuggestedAction = %q, want %q", got, domain.ActionNoop)
	}

	// --send must still send nothing: a noop is learned, never typed.
	if err := app.Confirm(ctx, id, true); err != nil {
		t.Fatalf("confirming a decline: %v", err)
	}
	if len(fake.inputs) != 0 {
		t.Errorf("a confirmed decline must send nothing, sent %v", fake.inputs)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "tasks", name+".md")); !os.IsNotExist(err) {
		t.Error("a confirmed decline must not write a tasks file")
	}
	cfg, _ := config.Load(app.ConfigPath)
	if len(cfg.TaskSources) != 0 {
		t.Errorf("a confirmed decline must not register a task source, got %d", len(cfg.TaskSources))
	}
	// The learned action is the sentinel, so the raw text never reaches a pane.
	corrs, err := st.UnprocessedCorrections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var mine []domain.CorrectionRecord
	for _, c := range corrs {
		if c.AuditID == id {
			mine = append(mine, c)
		}
	}
	if len(mine) != 1 {
		t.Fatalf("want 1 recorded correction for the escalation, got %d", len(mine))
	}
	if mine[0].CorrectedAction != domain.ActionNoop {
		t.Errorf("learned action = %q, want %q", mine[0].CorrectedAction, domain.ActionNoop)
	}
	if mine[0].Sent {
		t.Error("a confirmed decline must not be recorded as delivered")
	}
}

// TestConfigFieldsNeverRenderEnvValues guards the secrecy rule for the
// per-command LLM environment: the field registry is rendered verbatim by the
// TUI config screen and `hap config fields`, so no inline env VALUE may be
// reachable through it. Only the `.env` paths are registered.
func TestConfigFieldsNeverRenderEnvValues(t *testing.T) {
	cfg := config.Default()
	const secret = "sk-ant-supersecret"
	cfg.LLM.Env = map[string]string{"ANTHROPIC_API_KEY": secret}
	cfg.LLM.CommandEnv = map[string]string{"ANTHROPIC_API_KEY": secret}
	cfg.LLM.CommandStartEnv = map[string]string{"ANTHROPIC_API_KEY": secret}
	cfg.LLM.GenerateTaskEnv = map[string]string{"ANTHROPIC_API_KEY": secret}
	cfg.LLM.GenerateTaskStartEnv = map[string]string{"ANTHROPIC_API_KEY": secret}
	cfg.LLM.CommandEnvFile = "/etc/hap/consult.env"

	for _, key := range frontend.ConfigFieldKeys {
		if got := frontend.FieldValue(cfg, key); strings.Contains(got, secret) {
			t.Errorf("field %q rendered an env value: %q", key, got)
		}
	}
	// The path itself is not a secret and must stay visible.
	if got := frontend.FieldValue(cfg, "llm.command_env_file"); got != "/etc/hap/consult.env" {
		t.Errorf("llm.command_env_file = %q, want the configured path", got)
	}
	if got := frontend.FieldValue(cfg, "llm.env_file"); got != "(none)" {
		t.Errorf("unset env file = %q, want a clear placeholder", got)
	}
}
