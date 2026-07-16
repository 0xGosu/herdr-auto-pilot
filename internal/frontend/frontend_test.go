package frontend_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	// "send next declared task: " suggestion (the SuggestedAction /
	// materializeForSend contract), while recording the symbolic action.
	app, st := testApp(t)
	fake := &fakeHerdr{}
	app.Herdr = fake
	ctx := context.Background()

	prompt := domain.DeclaredTask{Task: "step two", Path: "/docs/tasks.md"}.Prompt()
	id, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w1:p1", SituationType: domain.SituationIdle, Trigger: "t",
		Action: "escalated", Status: "escalated",
		Suggestion: "send next declared task: " + prompt, CreatedAt: time.Now(),
	})
	if err := app.Confirm(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	corr, _ := st.UnprocessedCorrections(ctx)
	if len(corr) != 1 || corr[0].CorrectedAction != domain.ActionNextDeclaredTask {
		t.Errorf("confirm should record the symbolic declared-task action: %+v", corr)
	}
	if len(fake.inputs) != 1 || fake.inputs[0] != prompt {
		t.Errorf("delivered %v, want the rendered prompt %q", fake.inputs, prompt)
	}
	if len(fake.panes) != 1 || fake.panes[0] != "w1:p1" {
		t.Errorf("delivered to %v, want the audit's agent pane", fake.panes)
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

	// tasks.md written with a single in-progress item.
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
	// send=false establishes the source and file but delivers nothing.
	app, st := testApp(t)
	fake := &fakeHerdr{}
	app.Herdr = fake
	app.StateDir = t.TempDir()
	ctx := context.Background()

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
}

func TestConfirmGeneratedMultipleTasksWritesChecklist(t *testing.T) {
	// A multiline suggestion (a Markdown checklist from the LLM) is normalized:
	// the file lists the first task in-progress "[-]" and the rest pending
	// "[ ]", and ONLY the first task is sent to the agent.
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
	if err := app.Confirm(ctx, id, true); err == nil {
		t.Fatal("confirming a stale suggestion for a working agent must fail")
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
		{"llm.rewrite_command", `claude -p "rewrite: {text}" --model haiku`, false},
		{"llm.rewrite_command_start", `claude -p "first rewrite: {text}"`, false},
		{"llm.rewrite_command", `claude -p "unbalanced`, true},
		{"llm.rewrite_timeout_seconds", "45", false},
		{"llm.rewrite_timeout_seconds", "zero", true},
		{"llm.rewrite_fallback_template", "Act on: {original_text}", false},
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
	if len(cfg.LLM.RewriteCommand) != 5 || cfg.LLM.RewriteCommand[2] != "rewrite: {text}" {
		t.Errorf("llm.rewrite_command quote handling: %v", cfg.LLM.RewriteCommand)
	}
	if len(cfg.LLM.CommandStart) != 5 || cfg.LLM.CommandStart[2] != "first: {agent_name}" {
		t.Errorf("llm.command_start quote handling: %v", cfg.LLM.CommandStart)
	}
	if len(cfg.LLM.RewriteCommandStart) != 3 || cfg.LLM.RewriteCommandStart[2] != "first rewrite: {text}" {
		t.Errorf("llm.rewrite_command_start quote handling: %v", cfg.LLM.RewriteCommandStart)
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
	if got := frontend.FieldValue(config.Config{}, "llm.rewrite_command_start"); got != "(inherits rewrite_command)" {
		t.Errorf("empty rewrite_command_start display = %q, want inherit placeholder", got)
	}
	if got := frontend.FieldValue(config.Config{}, "llm.task_generate_command_start"); got != "(inherits task_generate_command)" {
		t.Errorf("empty task_generate_command_start display = %q, want inherit placeholder", got)
	}
	if got := frontend.FieldValue(config.Config{}, "llm.task_generate_timeout_seconds"); got != "(inherits timeout_seconds)" {
		t.Errorf("empty task_generate_timeout_seconds display = %q, want inherit placeholder", got)
	}
	if cfg.LLM.RewriteTimeoutSeconds != 45 ||
		cfg.LLM.RewriteFallbackTemplate != "Act on: {original_text}" {
		t.Errorf("rewrite keys not persisted: %+v", cfg.LLM)
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
		"confidence_thresholds.minimum":           "0.55",
		"confidence_thresholds.idle":              "0.70",
		"confidence_thresholds.approval":          "0.85",
		"confidence_thresholds.choice":            "0.85",
		"confidence_thresholds.error":             "0.90",
		"confidence_thresholds.inferred_task_bar": "0.95",
		"learning.graduation_n":                   "5",
		"embedding.pane_salient_chars":            "800",
		"limits.max_consecutive_auto_prompts":     "5",
		"limits.max_auto_prompts_per_minute":      "10",
		"limits.max_error_retries":                "2",
		"safety.disable_never_auto_seed_patterns": "true",
		"llm.command":                             `claude -p "decide"`,
		"llm.command_start":                       `claude -p "first: decide"`,
		"llm.timeout_seconds":                     "60",
		"llm.auto_act_confidence_threshold":       "70",
		"llm.pane_excerpt_chars":                  "4000",
		"llm.rewrite_command":                     `claude -p "rewrite: {text}"`,
		"llm.rewrite_command_start":               `claude -p "first rewrite: {text}"`,
		"llm.rewrite_timeout_seconds":             "45",
		"llm.rewrite_fallback_template":           "Act on: {original_text}",
		"llm.task_generate_command":               `claude -p "suggest a task"`,
		"llm.task_generate_command_start":         `claude -p "first suggest a task"`,
		"llm.task_generate_timeout_seconds":       "45",
		"embedding.disabled":                      "false",
		"embedding.model_path":                    "/models/custom.gguf",
		"embedding.similarity_threshold":          "0.90",
		"embedding.bm25_min_score":                "0.35",
		"embedding.gpu_layers":                    "0",
		"embedding.model_context_window":          "512",
		"tui.max_content_width":                   "140",
		"tui.max_content_height":                  "12",
		"tui.theme":                               "dark",
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
	// The default (999) renders with a "never" label, not a bare number.
	def := config.Default()
	got := frontend.FieldValue(def, "llm.auto_act_confidence_threshold")
	if !strings.Contains(got, "never") || !strings.Contains(got, "999") {
		t.Errorf("default threshold should show a never label, got %q", got)
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
		"llm.command":                     true,
		"llm.command_start":               true,
		"llm.rewrite_command":             true,
		"llm.rewrite_command_start":       true,
		"llm.rewrite_fallback_template":   true,
		"llm.task_generate_command":       true,
		"llm.task_generate_command_start": true,
		"embedding.model_path":            true,
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

	if err := app.RemoveTaskSource(ctx, 0, "/wrong/path.md"); err == nil {
		t.Error("mismatched expected path must refuse removal")
	}
	if err := app.RemoveTaskSource(ctx, 0, "/tmp/tasks.md"); err != nil {
		t.Fatal(err)
	}
	if err := app.RemoveTaskSource(ctx, 0, "/tmp/tasks.md"); err == nil {
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
}

func (f *fakeKeyHerdr) SendKey(_ context.Context, paneID, key string) error {
	f.keys = append(f.keys, key)
	if len(f.keyScript) > 0 && len(f.keyScriptFrames) > 0 && key == f.keyScript[0] {
		f.keyScript = f.keyScript[1:]
		f.pane = f.keyScriptFrames[0]
		f.keyScriptFrames = f.keyScriptFrames[1:]
	}
	return nil
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
	fake := &fakeKeyHerdr{fakeHerdr: fakeHerdr{
		pane: "←  ☐ Q one  ☐ Q two  ✔ Submit  →\n\nWhich backend?\n❯ 1. sqlite\n  2. postgres\n\nEnter to select · ↑/↓ to navigate · Tab to switch questions · Esc to cancel\n",
	}}
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
