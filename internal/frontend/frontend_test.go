package frontend_test

import (
	"bytes"
	"context"
	"encoding"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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
	"github.com/BurntSushi/toml"
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

	changed, err := app.Pause(ctx)
	if err != nil || !changed {
		t.Fatalf("pause: changed=%v err=%v", changed, err)
	}
	stat, err := app.GetStatus(ctx)
	if err != nil || !stat.Paused {
		t.Fatalf("pause not reflected: %+v %v", stat, err)
	}

	changed, err = app.Resume(ctx)
	if err != nil || !changed {
		t.Fatalf("resume: changed=%v err=%v", changed, err)
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

// A pause while already paused (or a resume while already running) is a
// no-op: reported as changed=false and recording NO kill event, so pressing
// "p"/"r" repeatedly cannot flood the history.
func TestPauseResumeNoOpsRecordNoKillEvent(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()

	// Resume before any pause: nothing to lift.
	if changed, err := app.Resume(ctx); err != nil || changed {
		t.Fatalf("resume on a never-paused daemon: changed=%v err=%v, want a no-op", changed, err)
	}
	if events, _ := st.KillEvents(ctx, 10); len(events) != 0 {
		t.Fatalf("no-op resume recorded %d kill events, want 0", len(events))
	}

	if changed, err := app.Pause(ctx); err != nil || !changed {
		t.Fatalf("first pause: changed=%v err=%v", changed, err)
	}
	if changed, err := app.Pause(ctx); err != nil || changed {
		t.Fatalf("second pause: changed=%v err=%v, want a no-op", changed, err)
	}
	if stat, err := app.GetStatus(ctx); err != nil || !stat.Paused {
		t.Fatalf("no-op pause must leave the daemon paused: %+v %v", stat, err)
	}

	if changed, err := app.Resume(ctx); err != nil || !changed {
		t.Fatalf("first resume: changed=%v err=%v", changed, err)
	}
	if changed, err := app.Resume(ctx); err != nil || changed {
		t.Fatalf("second resume: changed=%v err=%v, want a no-op", changed, err)
	}

	events, _ := st.KillEvents(ctx, 10)
	if len(events) != 2 {
		t.Errorf("kill history rows = %d, want only the two real transitions", len(events))
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

func TestConfirmWithoutSuggestionExplainsItselfAndOffersTheWayOut(t *testing.T) {
	// The four safety vetoes escalate without a suggestion on purpose, so
	// confirm must refuse — but the refusal is all the operator sees. "carries
	// no suggestion to confirm" reads as a broken plugin: it names neither the
	// control that fired nor the command that answers the escalation, and the
	// operator is left with a pending item they can see is answerable.
	app, st := testApp(t)
	ctx := context.Background()
	id, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "a1", SituationType: domain.SituationApproval, Trigger: "t",
		Action: "escalated", Status: "escalated", Suggestion: "",
		Rationale: "[never_auto_match] pattern (?i)\\brm\\s+-rf\\b matched \"rm -rf build\" (source=seed)",
		CreatedAt: time.Now(),
	})
	err := app.Confirm(ctx, id, false)
	if err == nil {
		t.Fatal("a suggestion-less escalation must not be confirmable")
	}
	for _, want := range []string{
		"never_auto_match", // which control fired
		fmt.Sprintf("hap resolve %d --action TEXT --send", id), // how to answer it
		fmt.Sprintf("hap dismiss %d", id),                      // how to drop it
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal must mention %q, got: %v", want, err)
		}
	}
}

func TestConfirmRefusalOnlyBlamesAControlThatActuallyWithheld(t *testing.T) {
	// A variance guard over an unfamiliar option set escalates with an empty
	// suggestion because NOTHING resolved, not because a control held one back.
	// Saying otherwise sends the operator hunting for a safety rule to relax.
	app, st := testApp(t)
	ctx := context.Background()
	id, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "a1", SituationType: domain.SituationChoice, Trigger: "t",
		Action: "escalated", Status: "escalated", Suggestion: "",
		Rationale: "[variance_guard] contradictory history; unfamiliar_options",
		CreatedAt: time.Now(),
	})
	err := app.Confirm(ctx, id, false)
	if err == nil {
		t.Fatal("a suggestion-less escalation must not be confirmable")
	}
	if strings.Contains(err.Error(), "on purpose") {
		t.Errorf("only the deliberate vetoes may be blamed, got: %v", err)
	}
	if !strings.Contains(err.Error(), "no action could be resolved") {
		t.Errorf("refusal must say nothing resolved, got: %v", err)
	}
}

func TestConfirmWithoutSuggestionOrReasonTagStillOffersTheWayOut(t *testing.T) {
	// A rationale with no "[reason]" tag (legacy rows, LLM-authored text) must
	// still produce an actionable refusal rather than a bare tag-less sentence.
	app, st := testApp(t)
	ctx := context.Background()
	id, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "a1", SituationType: domain.SituationApproval, Trigger: "t",
		Action: "escalated", Status: "escalated", Suggestion: "",
		Rationale: "something went sideways", CreatedAt: time.Now(),
	})
	err := app.Confirm(ctx, id, false)
	if err == nil {
		t.Fatal("a suggestion-less escalation must not be confirmable")
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("hap resolve %d --action TEXT --send", id)) {
		t.Errorf("refusal must still name the resolve command, got: %v", err)
	}
}

// fakeHerdr captures Send calls for confirm/resolve delivery assertions.

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
	// carrying the just-registered source's index (position 0) so the fallback
	// selector in the prompt is real, not the agent-name stand-in.
	wantPrompt := domain.DeclaredTask{Task: taskText, Path: path, AgentName: name, SourceIndex: "0"}.Prompt()
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
		Task: "Investigate the flaky auth test", Path: path, AgentName: name, SourceIndex: "0",
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

func TestConfirmGeneratedMultipleListsWritesOnlyLastList(t *testing.T) {
	// The suggestion carries the model's RAW output, which may hold several
	// Markdown lists — options it weighed, then the work it settled on. Only the
	// LAST list is real work, so only its items reach the checklist and only its
	// first item is sent. The daemon validated the same raw text with the same
	// parser, so the two sides cannot disagree about which list won.
	app, st := testApp(t)
	fake := &fakeHerdr{}
	app.Herdr = fake
	stateDir := t.TempDir()
	app.StateDir = stateDir
	ctx := context.Background()

	name, _ := st.EnsureAgentName(ctx, "w1:p1")
	suggestion := domain.SuggestTaskPrefix +
		"I weighed two approaches:\n" +
		"\n" +
		"- Rewrite the parser from scratch\n" +
		"- Patch the existing regex\n" +
		"\n" +
		"Final tasks:\n" +
		"\n" +
		"- [ ] Add multi-list handling\n" +
		"- [ ] Cover it with unit tests"
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
	want := "- [-] 1. Add multi-list handling\n- [ ] 2. Cover it with unit tests\n"
	if !strings.Contains(string(body), want) {
		t.Errorf("tasks file = %q, want only the last list %q", body, want)
	}
	// Nothing from the superseded list may be written as work.
	for _, dropped := range []string{"Rewrite the parser from scratch", "Patch the existing regex", "I weighed two approaches"} {
		if strings.Contains(string(body), dropped) {
			t.Errorf("tasks file must not carry the superseded list item %q, got %q", dropped, body)
		}
	}
	wantPrompt := domain.DeclaredTask{
		Task: "Add multi-list handling", Path: path, AgentName: name, SourceIndex: "0",
	}.Prompt()
	if len(fake.inputs) != 1 || fake.inputs[0] != wantPrompt {
		t.Errorf("delivered %v, want only the last list's first task as %q", fake.inputs, wantPrompt)
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
	wantPrompt := domain.DeclaredTask{Task: taskText, Path: path, AgentName: name, SourceIndex: "0"}.Prompt()
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
	wantPrompt := domain.DeclaredTask{Task: "Investigate the flaky auth test", Path: path, AgentName: name, SourceIndex: "0"}.Prompt()
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
	if _, err := app.SetThreshold(ctx, "approval", 0.93); err != nil {
		t.Fatal(err)
	}
	cfg, err := app.Config()
	if err != nil || cfg.ConfidenceThresholds.Approval != 0.93 {
		t.Fatalf("threshold not persisted: %+v %v", cfg.ConfidenceThresholds, err)
	}
	if _, err := app.SetThreshold(ctx, "approval", 1.5); err == nil {
		t.Error("out-of-range threshold must be rejected")
	}
	if _, err := app.SetThreshold(ctx, "bogus", 0.5); err == nil {
		t.Error("unknown situation must be rejected")
	}
	if _, err := app.SetThreshold(ctx, "minimum", 0.55); err != nil {
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

// seedMatcherFires rebuilds the never-auto matcher the daemon would build from
// the app's current config and reports whether content still trips a rule.
func seedMatcherFires(t *testing.T, app *frontend.App, content string) bool {
	t.Helper()
	cfg, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	list, errs := domain.NewNeverAutoList(!cfg.Safety.DisableNeverAutoSeedPatterns,
		cfg.Safety.DisabledSeedPatterns, cfg.Safety.NeverAutoPatterns, nil)
	if len(errs) > 0 {
		t.Fatalf("matcher compile: %v", errs)
	}
	_, matched := list.Match("claude", content)
	if !matched {
		_, matched = list.SuspectedIrreversible("claude", content)
	}
	return matched
}

func TestDisableAndEnableSeedRule(t *testing.T) {
	app, _ := testApp(t)
	ctx := context.Background()

	// Pick the no-undo heuristic — the one that over-fires on words like
	// "unrecoverable" — by locating it in the shipped seed list.
	seeds := domain.SeedNeverAutoRules()
	pattern := ""
	for _, r := range seeds {
		if strings.Contains(r.Pattern, "unrecoverabl") {
			pattern = r.Pattern
			break
		}
	}
	if pattern == "" {
		t.Fatal("no-undo heuristic seed rule not found")
	}

	if !seedMatcherFires(t, app, "This action cannot be undone") {
		t.Fatal("heuristic must fire before it is disabled")
	}

	if err := app.DisableSeedRule(ctx, pattern); err != nil {
		t.Fatal(err)
	}
	// Disabling again is a no-op, not a duplicate.
	if err := app.DisableSeedRule(ctx, pattern); err != nil {
		t.Fatal(err)
	}
	cfg, _ := app.Config()
	if len(cfg.Safety.DisabledSeedPatterns) != 1 || cfg.Safety.DisabledSeedPatterns[0] != pattern {
		t.Fatalf("disabled set wrong: %v", cfg.Safety.DisabledSeedPatterns)
	}
	if seedMatcherFires(t, app, "This action cannot be undone") {
		t.Error("disabled heuristic must not fire")
	}
	// Sibling rules stay active.
	if !seedMatcherFires(t, app, "DROP TABLE users") {
		t.Error("disabling one seed rule must not disable the others")
	}

	// A pattern that is not a shipped seed rule must be refused, so a bogus id
	// can never write an arbitrary string into the disable list.
	if err := app.DisableSeedRule(ctx, "not-a-seed-pattern"); err == nil {
		t.Error("a non-seed pattern must be refused")
	}

	if err := app.EnableSeedRule(ctx, pattern); err != nil {
		t.Fatal(err)
	}
	cfg, _ = app.Config()
	if len(cfg.Safety.DisabledSeedPatterns) != 0 {
		t.Errorf("re-enable must clear the disabled entry: %v", cfg.Safety.DisabledSeedPatterns)
	}
	if !seedMatcherFires(t, app, "This action cannot be undone") {
		t.Error("re-enabled heuristic must fire again")
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
		{"escalations.auto_accept.enabled", "true", false},
		{"escalations.auto_accept.enabled", "yes please", true},
		{"escalations.auto_accept.approval", "15m", false},
		{"escalations.auto_accept.approval", "1h30m", false},
		// "0" disables one type explicitly; clearing restores its default.
		{"escalations.auto_accept.idle", "0", false},
		{"escalations.auto_accept.idle", "", false},
		// Below the 1-minute sweep granularity: rejected, never rounded, or the
		// setting would silently not mean what the operator typed.
		{"escalations.auto_accept.choice", "30s", true},
		{"escalations.auto_accept.error", "soon", true},
		{"escalations.auto_accept.error", "-5m", true},
		{"llm.timeout_seconds", "90", false},
		{"llm.auto_act_confidence_threshold", "70", false},
		{"llm.auto_act_confidence_threshold", "-1", true},
		{"llm.auto_act_confidence_threshold", "maybe", true},
		{"llm.command", `claude -p "decide for me"`, false},
		{"llm.enable_rewrite_action", "true", false},
		{"llm.enable_rewrite_action", "maybe", true},
		{"llm.rewrite_action_fallback_template", "Act on: {original_text}", false},
		{"llm.task_generate_command", `claude -p "suggest: {agent_name}" --model haiku`, false},
		{"nonexistent.field", "1", true},
	}
	for _, c := range cases {
		_, err := app.SetField(ctx, c.key, c.value)
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
	if len(cfg.LLM.GenerateTaskCommand) != 5 || cfg.LLM.GenerateTaskCommand[2] != "suggest: {agent_name}" {
		t.Errorf("llm.task_generate_command quote handling: %v", cfg.LLM.GenerateTaskCommand)
	}
	// An unset optional key renders an inherit placeholder, not a blank cell.
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
		"confidence_thresholds.minimum":          "0.55",
		"confidence_thresholds.idle":             "0.70",
		"confidence_thresholds.approval":         "0.85",
		"confidence_thresholds.choice":           "0.85",
		"confidence_thresholds.error":            "0.90",
		"learning.graduation_n":                  "5",
		"learning.confirmation_weight":           "2.5",
		"embedding.pane_salient_chars":           "800",
		"limits.max_consecutive_auto_prompts":    "5",
		"limits.max_auto_prompts_per_minute":     "10",
		"limits.max_error_retries":               "2",
		"escalations.auto_accept.enabled":        "true",
		"escalations.auto_accept.approval":       "15m",
		"escalations.auto_accept.choice":         "15m",
		"escalations.auto_accept.error":          "30m",
		"escalations.auto_accept.idle":           "0",
		"escalations.auto_accept.unclassifiable": "0",
		// false on purpose: enabling is gated on runtime preconditions (10
		// graduated rules, llm.command) this shared fixture does not meet.
		// The enable path is exercised in fsp_test.go.
		"full_self_prompting.enabled": "false",
		// "true" on purpose, unlike the switch above: neither of these is
		// precondition-gated, so the accept path is exercised for real.
		"full_self_prompting.honour_limits":         "true",
		"full_self_prompting.accept_generated_task": "true",
		"safety.disable_never_auto_seed_patterns":   "true",
		"llm.command":                          `claude -p "decide"`,
		"llm.timeout_seconds":                  "60",
		"llm.auto_act_confidence_threshold":    "70",
		"llm.pane_excerpt_chars":               "4000",
		"llm.enable_rewrite_action":            "true",
		"llm.run_in_agent_cwd":                 "false",
		"llm.rewrite_action_fallback_template": "Act on: {original_text}",
		"llm.task_generate_command":            `claude -p "suggest a task"`,
		"llm.task_generate_timeout_seconds":    "45",
		"llm.learn_from_user_command":          `claude -p "record the lesson"`,
		"llm.learn_from_user_timeout_seconds":  "90",
		"llm.env_file":                         "/etc/hap/llm.env",
		"llm.command_env_file":                 "/etc/hap/consult.env",
		"llm.task_generate_command_env_file":   "/etc/hap/taskgen.env",
		"llm.learn_from_user_command_env_file": "/etc/hap/learn.env",
		"embedding.disabled":                   "false",
		"embedding.model_path":                 "/models/custom.gguf",
		"embedding.similarity_threshold":       "0.90",
		"embedding.bm25_min_score":             "0.35",
		"embedding.min_salient_chars":          "120",
		"embedding.model_context_window":       "512",
		"embedding.embed_timeout_ms":           "8000",
		"embedding.warm_timeout_ms":            "120000",
		"embedding.bm25_highbar_score":         "0.80",
		"logging.level":                        "warn",
		"logging.max_size_mb":                  "32",
		// github_gist and not local_fs on purpose: the sample doubles as the
		// SetField exercise, and setting the NON-default is what proves the
		// enum branch is reachable at all.
		"task_source_provider.provider":            "github_gist",
		"task_source_provider.env_file":            "/etc/hap/task_source.env",
		"task_source_provider.timeout_seconds":     "20",
		"task_source_provider.refresh_seconds":     "30",
		"task_source_provider.github_gist.gist_id": "3f2a1b9c4d5e6f708192a3b4c5d6e7f8",
		// Non-zero on purpose: 0 is a valid setting here ("never prune") but
		// this sample also feeds the FieldValue round trip, and a real day
		// count is the case worth exercising. The explicit-0 path has its own
		// test in internal/config.
		"logging.audit_excerpt_retention_days": "30",
		"tui.palette.title":                    "205",
		"tui.palette.section":                  "63",
		"tui.palette.error":                    "#ff5faf",
		"tui.palette.ok":                       "42",
		"tui.palette.paused":                   "214",
		"tui.palette.running":                  "39",
		"tui.palette.warn":                     "220",
		"tui.palette.help":                     "244",
		"tui.max_content_width":                "140",
		"tui.max_content_height":               "12",
		"tui.theme":                            "dark",
		"tui.terminal_bell":                    "true",
		"tui.herdr_notification":               "false",
		"tui.disable_check_for_update":         "true",
		"tui.max_instances":                    "2",
		"cli.ai_agent_friendly_output":         "false",
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
		if _, err := app.SetField(ctx, key, samples[key]); err != nil {
			t.Errorf("SetField(%s, %q) rejected a valid value: %v", key, samples[key], err)
		}
	}
}

func TestAutoActConfidenceThresholdFieldDisplay(t *testing.T) {
	// The default (85) is a reachable 0-100 threshold, so it renders as a bare
	// number, not the "never" label.
	def := config.Default()
	got := frontend.FieldValue(def, "llm.auto_act_confidence_threshold")
	if got != "85" {
		t.Errorf("default threshold display = %q, want a bare 85", got)
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
	if _, err := app.SetField(ctx, "llm.auto_act_confidence_threshold", "0"); err != nil {
		t.Fatalf("threshold 0 (act on any score) must be accepted: %v", err)
	}
	cfg, _ := app.Config()
	if cfg.LLM.AutoActConfidenceThreshold != 0 {
		t.Errorf("SetField did not persist 0, got %d", cfg.LLM.AutoActConfidenceThreshold)
	}
	if _, err := app.SetField(ctx, "llm.auto_act_confidence_threshold", "-5"); err == nil {
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
	if _, err := app.SetField(ctx, "embedding.pane_salient_chars", "1000"); err != nil {
		t.Fatal(err)
	}
	cfg, _ := app.Config()
	if cfg.Embedding.PaneSalientChars != 1000 {
		t.Errorf("SetField did not persist pane_salient_chars, got %d", cfg.Embedding.PaneSalientChars)
	}
	if _, err := app.SetField(ctx, "embedding.pane_salient_chars", "0"); err == nil {
		t.Error("pane_salient_chars must reject 0 (use omission for the default)")
	}
}

// TestFieldTUIEditableClassification pins CR-036: free-text fields (argv
// templates, template strings, paths) are read-only in the TUI, everything
// else in the registry is editable, and unknown keys are never editable.
func TestFieldTUIEditableClassification(t *testing.T) {
	readOnly := map[string]bool{
		"llm.command":                          true,
		"llm.rewrite_action_fallback_template": true,
		"llm.task_generate_command":            true,
		"llm.learn_from_user_command":          true,
		"embedding.model_path":                 true,
	}
	for _, f := range frontend.ConfigFields {
		// Assert the DECLARED flag, not the accessor's output. A hidden
		// field's FieldTUIEditable is forced false, so deriving the
		// expectation from the accessor would leave TUIEditable unpinned
		// for exactly the fields where it matters — it is the value that
		// takes effect the day one is un-hidden.
		if f.TUIEditable == readOnly[f.Key] {
			t.Errorf("%s: TUIEditable=%v contradicts the CR-036 read-only classification", f.Key, f.TUIEditable)
		}
		// The accessor folds in TUIHidden: the TUI cannot edit a row it
		// does not render.
		want := f.TUIEditable && !f.TUIHidden
		if got := frontend.FieldTUIEditable(f.Key); got != want {
			t.Errorf("FieldTUIEditable(%s) = %v, want %v", f.Key, got, want)
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

// TestTUIHiddenConfigFields: the advanced knobs are dropped from the TUI's
// display list only. They must stay in ConfigFieldKeys so config.toml,
// `hap config fields`, and `hap config set` keep working on them.
func TestTUIHiddenConfigFields(t *testing.T) {
	hidden := map[string]bool{
		"llm.pane_excerpt_chars":               true,
		"llm.enable_rewrite_action":            true,
		"llm.run_in_agent_cwd":                 true,
		"llm.rewrite_action_fallback_template": true,
		"llm.env_file":                         true,
		"llm.command_env_file":                 true,
		"llm.task_generate_command_env_file":   true,
		"llm.learn_from_user_command_env_file": true,
		"embedding.pane_salient_chars":         true,
		"embedding.warm_timeout_ms":            true,
		// The env file holds a token, so it follows the llm.*_env_file rule:
		// registered (a path is not a secret and `hap config set` must reach
		// it) but off the TUI's Config tab. The two timing knobs are tuned once
		// if ever, and only matter for a remote provider.
		"task_source_provider.env_file":        true,
		"task_source_provider.timeout_seconds": true,
		"task_source_provider.refresh_seconds": true,
		// Eight color strings would bury the settings a TUI operator actually
		// reaches for, but they stay registered so `hap config set` reaches them.
		"tui.palette.title":   true,
		"tui.palette.section": true,
		"tui.palette.error":   true,
		"tui.palette.ok":      true,
		"tui.palette.paused":  true,
		"tui.palette.running": true,
		"tui.palette.warn":    true,
		"tui.palette.help":    true,
	}
	all := make(map[string]bool, len(frontend.ConfigFieldKeys))
	for _, key := range frontend.ConfigFieldKeys {
		all[key] = true
		if got := frontend.FieldTUIHidden(key); got != hidden[key] {
			t.Errorf("FieldTUIHidden(%s) = %v, want %v", key, got, hidden[key])
		}
	}
	for key := range hidden {
		if !all[key] {
			t.Errorf("hidden key %q is gone from ConfigFieldKeys — it must stay settable via `hap config set`", key)
		}
	}
	if frontend.FieldTUIHidden("nonexistent.field") {
		t.Error("unknown key must not report as hidden")
	}

	// TUIConfigFieldKeys is exactly ConfigFieldKeys minus the hidden ones,
	// in the same order.
	var want []string
	for _, key := range frontend.ConfigFieldKeys {
		if !hidden[key] {
			want = append(want, key)
		}
	}
	if !slices.Equal(frontend.TUIConfigFieldKeys, want) {
		t.Errorf("TUIConfigFieldKeys = %v, want %v", frontend.TUIConfigFieldKeys, want)
	}

	// Hidden fields still round-trip through SetField.
	app, _ := testApp(t)
	if _, err := app.SetField(context.Background(), "embedding.warm_timeout_ms", "90000"); err != nil {
		t.Fatalf("SetField on a hidden key must still work: %v", err)
	}
	cfg, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Embedding.WarmTimeoutMs != 90000 {
		t.Errorf("hidden field not persisted: %d", cfg.Embedding.WarmTimeoutMs)
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
		{"tui.herdr_notification", "true", false},
		{"tui.herdr_notification", "false", false},
		{"tui.herdr_notification", "sometimes", true},
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
	}
	for _, c := range cases {
		_, err := app.SetField(ctx, c.key, c.value)
		if (err != nil) != c.wantErr {
			t.Errorf("SetField(%s, %q) error = %v, wantErr %v", c.key, c.value, err, c.wantErr)
		}
	}

	// The unknown-theme error names the valid themes.
	_, err := app.SetField(ctx, "tui.theme", "solarized")
	if err == nil {
		t.Fatal("unknown theme must be rejected")
	}
	for _, name := range config.ValidThemes {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("theme error should list valid name %q, got: %v", name, err)
		}
	}

	// Case-insensitive theme names normalize to lowercase on persist.
	if _, err := app.SetField(ctx, "tui.theme", "DARK"); err != nil {
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
	if _, err := app.SetField(ctx, "tui.theme", ""); err != nil {
		t.Fatalf("empty theme should reset: %v", err)
	}
	if cfg, _ = app.Config(); cfg.TUI.Theme != "" {
		t.Errorf("empty theme should persist as \"\", got %q", cfg.TUI.Theme)
	}

	// End each key on a NON-zero accepted value so persistence is positively
	// asserted (a validator that forgot the assignment would otherwise pass).
	if _, err := app.SetField(ctx, "tui.max_content_width", "140"); err != nil {
		t.Fatal(err)
	}
	if _, err := app.SetField(ctx, "tui.max_content_height", "12"); err != nil {
		t.Fatal(err)
	}
	if _, err := app.SetField(ctx, "safety.disable_never_auto_seed_patterns", "true"); err != nil {
		t.Fatal(err)
	}
	if _, err := app.SetField(ctx, "llm.task_generate_timeout_seconds", "30"); err != nil {
		t.Fatal(err)
	}
	// Ends on false — the non-default value, so a validator that forgot the
	// assignment cannot pass by inheriting the true default.
	if _, err := app.SetField(ctx, "tui.herdr_notification", "false"); err != nil {
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
	if cfg.TUI.HerdrNotification {
		t.Error("tui.herdr_notification = true, want false (assignment not persisted)")
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
	if out := run("rules", "list"); !strings.Contains(out, "\tstrict\t") || !strings.Contains(out, "\theuristic\t") {
		t.Errorf("rules output: %q", out)
	}
	run("config", "set", "safety.disable_never_auto_seed_patterns", "true")
	if out := run("rules", "list"); !strings.Contains(out, "shipped never-auto rules disabled") ||
		strings.Contains(out, "\tstrict\t") || strings.Contains(out, "\theuristic\t") {
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
			"matched by `bm25_min_score` (bm25 0.42, text fallback)"},
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

	app, _ := testApp(t)
	cfg := config.Config{TaskSources: []config.TaskSource{
		{Agent: "brave-otter", Workspace: "w1", Path: good},
		{Agent: "codex", Path: missing},
		{Agent: "empty-path"},
		{Workspace: "*", Path: good}, // duplicate path, its own group
		{Agent: "quiet", Path: empty},
	}}
	groups := app.TaskGroups(cfg, frontend.Status{})
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
	// The message is now provider-aware: an empty path is a misconfiguration
	// under local_fs but the ordinary "one list per agent" form under a remote
	// provider, so the error says which case it is.
	if g := groups[2]; !strings.Contains(g.Err, "no path") || !strings.Contains(g.Err, "local_fs") {
		t.Errorf("empty path: Err=%q, want it to name the missing path and the provider", g.Err)
	}
	if g := groups[3]; g.Err != "" || len(g.Items) != 3 {
		t.Errorf("duplicate path: Err=%q items=%d, want an independent readable group", g.Err, len(g.Items))
	}
	if g := groups[4]; g.Err != "" || len(g.Items) != 0 {
		t.Errorf("readable file without checklist items: Err=%q items=%d, want no error and no items", g.Err, len(g.Items))
	}
}

func TestTaskGroupsEmptyConfig(t *testing.T) {
	app, _ := testApp(t)
	if groups := app.TaskGroups(config.Config{}, frontend.Status{}); len(groups) != 0 {
		t.Errorf("no task sources should yield no groups, got %d", len(groups))
	}
}

// derivedTemplateNote is the note a derived (one-list-per-matched-agent)
// source shows when the aggregate view cannot resolve it to a single list.
const derivedTemplateNote = "one list per matched agent"

func derivedGistCfg(t *testing.T, agentSelector string) config.Config {
	t.Helper()
	return config.Config{
		TaskSourceProvider: config.TaskSourceProvider{
			Provider: config.ProviderGitHubGist,
			// Absent on purpose: the token is read at USE time, so resolution
			// succeeds and the read fails fast — no network is ever dialed.
			EnvFile:    filepath.Join(t.TempDir(), "absent.env"),
			GitHubGist: config.GitHubGist{GistID: "3f2a1b9c"},
		},
		TaskSources: []config.TaskSource{{Agent: agentSelector}},
	}
}

// TestTaskListForIndexSelector pins the index selector against a REMOTE
// provider — the provider-independence the selector exists for: an explicit
// gist file resolves to its locator with no agent name (and no token read, so
// no network), a DERIVED source refuses toward the agent name (the index
// names a source, and its lists are per agent), and '#N' equals bare N.
func TestTaskListForIndexSelector(t *testing.T) {
	app, _ := testApp(t)
	cfg := derivedGistCfg(t, "claude") // source 0: derived (no file name)
	cfg.TaskSources = append(cfg.TaskSources, config.TaskSource{Agent: "x", Path: "shared.md"})
	if err := config.Save(app.ConfigPath, cfg); err != nil {
		t.Fatal(err)
	}

	locator, sourceIndex, err := app.TaskListFor("1", "")
	if err != nil {
		t.Fatal(err)
	}
	if want := "gist://3f2a1b9c/shared.md"; locator != want || sourceIndex != "1" {
		t.Fatalf("TaskListFor(\"1\") = (%q, %q), want (%q, \"1\")", locator, sourceIndex, want)
	}
	if hashed, _, err := app.TaskListFor("#1", ""); err != nil || hashed != locator {
		t.Fatalf("TaskListFor(\"#1\") = (%q, %v), want the same list as \"1\"", hashed, err)
	}

	if _, _, err := app.TaskListFor("0", ""); err == nil {
		t.Fatal("a derived source addressed by index must refuse")
	} else if !strings.Contains(err.Error(), "one list per matched agent") {
		t.Fatalf("derived-by-index error should say why and point at the agent name, got: %v", err)
	}

	if _, _, err := app.TaskListFor("5", ""); err == nil {
		t.Fatal("an out-of-range index must refuse")
	} else if !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("out-of-range error should say the source does not exist, got: %v", err)
	}
}

// TestTaskGroupsDerivedSourceSoleLiveMatchResolves: a derived source with
// exactly one live matching agent resolves to that agent's list — the case
// the Tasks tab used to render as a template note even though there was
// nothing to guess.
func TestTaskGroupsDerivedSourceSoleLiveMatchResolves(t *testing.T) {
	app, _ := testApp(t)
	cfg := derivedGistCfg(t, "brave-otter")
	st := frontend.Status{
		AgentsKnown: true,
		MonitoredAgents: []domain.AgentTransition{
			{AgentID: "t1", AgentType: "claude", WorkspaceID: "w1", Status: "idle"},
		},
		AgentNames: map[string]string{"t1": "brave-otter"},
	}
	groups := app.TaskGroups(cfg, st)
	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1", len(groups))
	}
	g := groups[0]
	if want := "gist://3f2a1b9c/brave-otter.md"; g.Locator != want {
		t.Fatalf("Locator=%q Err=%q, want the sole match's derived list %q", g.Locator, g.Err, want)
	}
	if g.ListAddress() != g.Locator {
		t.Fatalf("ListAddress()=%q, want the resolved locator %q — actions must never fall back to the empty Source.Path", g.ListAddress(), g.Locator)
	}
	// The list itself is unreadable here (no credentials), and that must
	// surface as the READ failure, not as the template note: the source DID
	// resolve.
	if strings.Contains(g.Err, derivedTemplateNote) {
		t.Fatalf("Err=%q still shows the per-agent template note after a sole live match", g.Err)
	}
	if g.Err == "" {
		t.Fatal("reading a gist list with no credentials should have failed")
	}
}

// TestTaskGroupsDerivedSourceStaysTemplateWhenAmbiguous pins every case where
// a derived source must KEEP the per-agent note: guessing a list here would
// show (and let the operator mutate) some other agent's list.
func TestTaskGroupsDerivedSourceStaysTemplateWhenAmbiguous(t *testing.T) {
	otter := domain.AgentTransition{AgentID: "t1", AgentType: "claude", WorkspaceID: "w1"}
	badger := domain.AgentTransition{AgentID: "t2", AgentType: "claude", WorkspaceID: "w1"}
	names := map[string]string{"t1": "brave-otter", "t2": "calm-badger"}
	cases := []struct {
		name     string
		selector string
		st       frontend.Status
	}{
		{"two live agents match a type selector", "claude", frontend.Status{
			AgentsKnown:     true,
			MonitoredAgents: []domain.AgentTransition{otter, badger},
			AgentNames:      names,
		}},
		// "no live agent matches" is deliberately NOT here: a source scoped to
		// an agent NAME resolves without that agent running (see
		// TestTaskGroupsDerivedSourceResolvesByItsNamedSelector). What stays
		// ambiguous with nothing live is a source that names no agent at all —
		// a workspace scope, which any number of agents can enter.
		{"workspace-scoped source, nothing live", "", frontend.Status{
			AgentsKnown:     true,
			MonitoredAgents: nil,
			AgentNames:      names,
		}},
		{"agent listing unknown", "brave-otter", frontend.Status{
			// A failed agent query is NOT "no agents": resolution must not
			// act on absence it cannot see.
			AgentsKnown:     false,
			MonitoredAgents: []domain.AgentTransition{otter},
			AgentNames:      names,
		}},
		{"sole match has no short name", "t1", frontend.Status{
			// Matched by pane id, but the derived file name is built from the
			// SHORT name — without one there is nothing to resolve to.
			AgentsKnown:     true,
			MonitoredAgents: []domain.AgentTransition{otter},
			AgentNames:      map[string]string{},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app, _ := testApp(t)
			groups := app.TaskGroups(derivedGistCfg(t, tc.selector), tc.st)
			if len(groups) != 1 {
				t.Fatalf("got %d groups, want 1", len(groups))
			}
			g := groups[0]
			if !strings.Contains(g.Err, derivedTemplateNote) {
				t.Fatalf("Err=%q, want the per-agent template note", g.Err)
			}
			if g.Locator != "" {
				t.Fatalf("Locator=%q, want none — an ambiguous derived source must not pick a list", g.Locator)
			}
		})
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
		path, "", "3", 1, `step one\nstep two`); err != nil {
		t.Fatal(err)
	}
	if len(h.sent) != 1 {
		t.Fatalf("expected one delivery, got %v", h.sent)
	}
	// The default prompt names the task, the agent, and the task-source index
	// fallback — never the file path, which the CLI's hints render instead
	// (and which under a remote provider would be a URL the agent cannot use).
	for _, want := range []string{"step one\nstep two", "brave-otter", "use the task-source index `3`"} {
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
	if err := app.SendTaskToAgent(ctx, "w1:p2", "claude", "n", path, "", "", 1, `step one\nstep two`); err == nil ||
		!strings.Contains(err.Error(), "no longer pending") {
		t.Errorf("completed task must refuse to send, got %v", err)
	}
	if err := app.SendTaskToAgent(ctx, "w1:p2", "claude", "n", path, "", "", 1, "different text"); err == nil ||
		!strings.Contains(err.Error(), "the checklist changed") {
		t.Errorf("rewritten task must refuse to send, got %v", err)
	}
	if len(h.sent) != 1 {
		t.Errorf("refused sends must not deliver, got %v", h.sent)
	}

	// Guards: no herdr / no pane.
	app.Herdr = nil
	if err := app.SendTaskToAgent(ctx, "w1:p2", "claude", "n", path, "", "", 1, "t"); err == nil {
		t.Error("nil herdr must refuse")
	}
	app.Herdr = h
	if err := app.SendTaskToAgent(ctx, "", "claude", "n", path, "", "", 1, "t"); err == nil {
		t.Error("empty pane must refuse")
	}
}

func TestSendTaskToAgentFoldsNestedDetail(t *testing.T) {
	// A manual send folds the RESERVED item's nested sub-items into the
	// delivered prompt (from the same locked snapshot as the reservation), while
	// the item is marked [-] by its single-line identity.
	app, _ := testApp(t)
	h := &sendCaptureHerdr{agents: idleAt("w1:p2")}
	app.Herdr = h
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tasks.md")
	content := "- [ ] 1. Build the widget\n" +
		"  - Wire the API\n" +
		"  - Acceptance: it renders\n" +
		"- [ ] 2. Later task\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := app.SendTaskToAgent(ctx, "w1:p2", "claude", "brave-otter", path, "", "", 1, "1. Build the widget"); err != nil {
		t.Fatal(err)
	}
	if len(h.sent) != 1 {
		t.Fatalf("expected one delivery, got %v", h.sent)
	}
	for _, want := range []string{"1. Build the widget", "- Wire the API", "- Acceptance: it renders"} {
		if !strings.Contains(h.sent[0], want) {
			t.Errorf("delivered prompt missing folded detail %q:\n%s", want, h.sent[0])
		}
	}
	// The second task's detail must NOT leak into task 1's delivery.
	if strings.Contains(h.sent[0], "Later task") {
		t.Errorf("a sibling task leaked into the folded content:\n%s", h.sent[0])
	}
	// Reserved by single-line identity; nested lines and the next task untouched.
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "- [-] 1. Build the widget") {
		t.Errorf("task should be marked in progress, got:\n%s", data)
	}
	if !strings.Contains(string(data), "  - Wire the API") || !strings.Contains(string(data), "- [ ] 2. Later task") {
		t.Errorf("nested detail and next task must be preserved, got:\n%s", data)
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
	if err := app.SendTaskToAgent(ctx, "w1:p2", "claude", "otter", path, "", "", 1, "work"); err == nil ||
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
	if err := app.SendTaskToAgent(ctx, "w1:p2", "claude", "otter", path, "", "", 1, "work"); err == nil ||
		!strings.Contains(err.Error(), "no longer live") {
		t.Errorf("a vanished agent must refuse, got %v", err)
	}
	// An unreadable agent list is not an idle agent: fail closed.
	app, path = newApp(t, nil)
	app.Herdr = &failingAgentsHerdr{}
	if err := app.SendTaskToAgent(ctx, "w1:p2", "claude", "otter", path, "", "", 1, "work"); err == nil ||
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
	if err := app.SendTaskToAgent(ctx, "w1:p2", "claude", "otter", path, "", "", 1, "work"); err != nil {
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
	if err := app2.SendTaskToAgent(ctx, "w1:p2", "claude", "otter", path2, "", "", 1, "work"); err == nil ||
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
	err := app.SendTaskToAgent(ctx, "w1:p2", "claude", "otter", path, "", "", 1, "work")
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
		"do {next_task_content} in {cwd}", "", 1, "work"); err != nil {
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
		"do {next_task_content} in {cwd}", "", 1, "work"); err != nil {
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

// TestAddTaskSourceReviewOption pins the opt-in review flag: it round-trips
// as an explicit value (the field is a *bool, so "explicitly off" must be
// distinguishable from "never decided" on disk), and an add that names neither
// flag lands with review off — the default.
func TestAddTaskSourceReviewOption(t *testing.T) {
	app, _ := testApp(t)
	ctx := context.Background()
	dir := t.TempDir()
	plain := filepath.Join(dir, "plain.md")
	on := filepath.Join(dir, "on.md")
	off := filepath.Join(dir, "off.md")

	if err := app.AddTaskSource(ctx, "quiet-fox", "", plain, ""); err != nil {
		t.Fatal(err)
	}
	if err := app.AddTaskSource(ctx, "busy-otter", "", on, "", frontend.ReviewBeforeAutoSend(true)); err != nil {
		t.Fatal(err)
	}
	if err := app.AddTaskSource(ctx, "calm-vole", "", off, "", frontend.ReviewBeforeAutoSend(false)); err != nil {
		t.Fatal(err)
	}
	reloaded, err := config.Load(app.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.TaskSources) != 3 {
		t.Fatalf("want 3 task sources, got %d", len(reloaded.TaskSources))
	}
	if reloaded.TaskSources[0].ReviewBeforeAutoSendEnabled() {
		t.Error("an add with no option must leave the review off — that is the default")
	}
	if !reloaded.TaskSources[1].ReviewBeforeAutoSendEnabled() {
		t.Error("LLMReview(true) did not round-trip through config.toml")
	}
	// Asserted on the POINTER, not the resolver: !LLMReviewEnabled() would also
	// pass for nil and would not prove an explicit false was written.
	if got := reloaded.TaskSources[2].EnableLLMReviewBeforeAutoSend; got == nil || *got {
		t.Errorf("LLMReview(false) must persist as an explicit false, got %v", got)
	}
}

// TestTaskSourceReviewAndAutoSendCompose is the inverse of the rule #253/#254
// had to impose: the two flags were mutually exclusive because the review ran
// upstream of domain.Decide and escalated when it declined, and one pending
// escalation bars an agent from the idle poll — so a reviewed auto-send source
// switched itself off. The review is now a pre-delivery filter that never
// escalates, so every write surface must ACCEPT the pair, in either order.
func TestTaskSourceReviewAndAutoSendCompose(t *testing.T) {
	app, _ := testApp(t)
	ctx := context.Background()
	dir := t.TempDir()

	// AddTaskSource is the one path where both arrive at once, in either order.
	for i, opts := range [][]frontend.TaskSourceOption{
		{frontend.ReviewBeforeAutoSend(true), frontend.AutoSendWhenIdle()},
		{frontend.AutoSendWhenIdle(), frontend.ReviewBeforeAutoSend(true)},
	} {
		path := filepath.Join(dir, fmt.Sprintf("both%d.md", i))
		if err := app.AddTaskSource(ctx, "composed", "", path, "", opts...); err != nil {
			t.Fatalf("AddTaskSource must accept both flags (order %d), got %v", i, err)
		}
	}
	reloaded, err := config.Load(app.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.TaskSources) != 2 {
		t.Fatalf("want 2 task sources, got %d", len(reloaded.TaskSources))
	}
	for i, src := range reloaded.TaskSources {
		if !src.ReviewBeforeAutoSendEnabled() || !src.EnableAutoSendTaskWhenIdle {
			t.Errorf("source %d lost a flag: %+v", i, src)
		}
	}

	// And both directions of turning the second flag on over a standing first.
	reviewed := filepath.Join(dir, "reviewed.md")
	if err := app.AddTaskSource(ctx, "reviewer", "", reviewed, "", frontend.ReviewBeforeAutoSend(true)); err != nil {
		t.Fatal(err)
	}
	handout := filepath.Join(dir, "handout.md")
	if err := app.AddTaskSource(ctx, "handout", "", handout, "", frontend.AutoSendWhenIdle()); err != nil {
		t.Fatal(err)
	}
	cfg, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	if err := app.SetTaskSourceAutoSend(ctx, 2, cfg.TaskSources[2], true); err != nil {
		t.Errorf("turning auto-send on over a reviewed source must be allowed, got %v", err)
	}
	if cfg, err = app.Config(); err != nil {
		t.Fatal(err)
	}
	if err := app.SetTaskSourceReviewBeforeAutoSend(ctx, 3, cfg.TaskSources[3], true); err != nil {
		t.Errorf("turning review on over an auto-send source must be allowed, got %v", err)
	}
	if reloaded, err = config.Load(app.ConfigPath); err != nil {
		t.Fatal(err)
	}
	for _, i := range []int{2, 3} {
		src := reloaded.TaskSources[i]
		if !src.ReviewBeforeAutoSendEnabled() || !src.EnableAutoSendTaskWhenIdle {
			t.Errorf("source %d did not end up with both flags: %+v", i, src)
		}
	}
}

// TestSetTaskSourceMaxTasksIsUngated guards the blast radius of the validation
// living in updateTaskSource: an edit that touches neither delivery-gate flag
// must never be refused because of them.
func TestSetTaskSourceMaxTasksIsUngated(t *testing.T) {
	app, _ := testApp(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tasks.md")
	if err := app.AddTaskSource(ctx, "handout", "", path, "", frontend.AutoSendWhenIdle()); err != nil {
		t.Fatal(err)
	}
	cfg, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	if err := app.SetTaskSourceMaxTasks(ctx, 0, cfg.TaskSources[0], 7); err != nil {
		t.Fatalf("an unrelated edit must not be gated on the exclusive pair, got %v", err)
	}
	reloaded, err := config.Load(app.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.TaskSources[0].MaxTasks != 7 {
		t.Errorf("max_tasks = %d, want 7", reloaded.TaskSources[0].MaxTasks)
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
// per-command LLM environment: the field registry is rendered verbatim by
// `hap config fields`, so no inline env VALUE may be reachable through it.
// Only the `.env` paths are registered.
func TestConfigFieldsNeverRenderEnvValues(t *testing.T) {
	cfg := config.Default()
	const secret = "sk-ant-supersecret"
	cfg.LLM.Env = map[string]string{"ANTHROPIC_API_KEY": secret}
	cfg.LLM.CommandEnv = map[string]string{"ANTHROPIC_API_KEY": secret}
	cfg.LLM.GenerateTaskEnv = map[string]string{"ANTHROPIC_API_KEY": secret}
	cfg.LLM.LearnFromUserEnv = map[string]string{"ANTHROPIC_API_KEY": secret}
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

// TestAuditStatusLabel: an operator scanning a list must be able to tell a
// machine's action from their own without opening the record.
func TestAuditStatusLabel(t *testing.T) {
	tests := []struct {
		name string
		rec  domain.AuditRecord
		want string
	}{
		{
			// The critical distinction: an auto-accept delivered a reply but
			// recorded NO learning event, so it must not read as "resolved".
			name: "auto-accepted is not an operator resolution",
			rec:  domain.AuditRecord{Status: domain.AuditStatusAutoAccepted},
			want: "auto-sent",
		},
		{
			// Same status, different cause. Both are automatic acceptances, but
			// one is a threshold expiring and the other is a mode the operator
			// switched on — an operator scanning the log needs to tell them
			// apart without opening each row.
			name: "full self-prompting is distinguishable from a timed auto-accept",
			rec:  domain.AuditRecord{Status: domain.AuditStatusAutoAccepted, WhileFSPModeOn: true},
			want: "fsp-sent",
		},
		{
			name: "transient claim renders rather than showing unknown",
			rec:  domain.AuditRecord{Status: domain.AuditStatusAutoAccepting},
			want: "sending",
		},
		{
			name: "operator dismissal",
			rec:  domain.AuditRecord{Status: "dismissed", Rationale: "[shadow_mode] learning"},
			want: "dismissed",
		},
		{
			name: "machine dismissal: the situation moved on",
			rec: domain.AuditRecord{Status: "dismissed",
				Rationale: "[shadow_mode] learning [auto_dismiss_stale] signature drifted"},
			want: "dism:stale",
		},
		{
			name: "machine dismissal: the agent is gone",
			rec: domain.AuditRecord{Status: "dismissed",
				Rationale: "[shadow_mode] learning [auto_dismiss_agent_gone]"},
			want: "dism:gone",
		},
		{
			name: "machine dismissal: delivery never succeeded",
			rec: domain.AuditRecord{Status: "dismissed",
				Rationale: "[shadow_mode] learning [auto_accept_failed] 3 attempts"},
			want: "dism:failed",
		},
		{"escalated is unchanged", domain.AuditRecord{Status: "escalated"}, "escalated"},
		{"resolved is unchanged", domain.AuditRecord{Status: "resolved"}, "resolved"},
		{"auto is unchanged", domain.AuditRecord{Status: "auto"}, "auto"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := frontend.AuditStatusLabel(tt.rec); got != tt.want {
				t.Errorf("AuditStatusLabel = %q, want %q", got, tt.want)
			}
			if n := len(frontend.AuditStatusLabel(tt.rec)); n > frontend.AuditStatusWidth {
				t.Errorf("label is %d wide, exceeding AuditStatusWidth=%d — it would shift the columns beside it",
					n, frontend.AuditStatusWidth)
			}
		})
	}
}

// TestAuditStatusLabelDistinguishesMachineFromOperator is the requirement in
// one assertion: no two authors, and no two machine reasons, may collide.
func TestAuditStatusLabelDistinguishesMachineFromOperator(t *testing.T) {
	labels := map[string]string{}
	for _, rec := range []domain.AuditRecord{
		{Status: "resolved"},
		{Status: domain.AuditStatusAutoAccepted},
		{Status: domain.AuditStatusAutoAccepted, WhileFSPModeOn: true},
		{Status: "dismissed", Rationale: "[shadow_mode] x"},
		{Status: "dismissed", Rationale: "[shadow_mode] x [auto_dismiss_stale] y"},
		{Status: "dismissed", Rationale: "[shadow_mode] x [auto_dismiss_agent_gone]"},
		{Status: "dismissed", Rationale: "[shadow_mode] x [auto_accept_failed] y"},
	} {
		l := frontend.AuditStatusLabel(rec)
		if prev, dup := labels[l]; dup {
			t.Errorf("label %q is shared by %q and %q", l, prev, rec.Rationale+"/"+rec.Status)
		}
		labels[l] = rec.Rationale + "/" + rec.Status
	}
}

// configKeysExemptFromRegistry are the config.toml keys deliberately NOT
// settable through `hap config set`. Every entry needs a reason; the default
// answer for a new key is to REGISTER it, not to add it here.
//
// Every entry today is a deprecated alias kept only so an existing config.toml
// still loads and migrates onto the canonical key. Listing one here keeps it
// out of the REGISTRY, which is what `hap config fields` prints and the TUI
// Config tab renders — an operator must never be shown the spelling we are
// migrating away from. A key may still RESOLVE when typed (see
// frontend.CanonicalConfigKey); resolving writes the canonical field, so the
// old spelling is never authored back into config.toml either way. They are
// listed explicitly rather than left to fall through the walk, because most are
// pointers and would otherwise be skipped for their SHAPE rather than for this
// reason.
var configKeysExemptFromRegistry = map[string]string{
	"llm.rewrite_fallback_template":           "deprecated alias for llm.rewrite_action_fallback_template",
	"llm.auto_act":                            "deprecated alias for llm.auto_act_confidence_threshold",
	"safety.disable_seed":                     "deprecated alias for safety.disable_never_auto_seed_patterns",
	"escalations.full_self_prompting.enabled": "deprecated alias for full_self_prompting.enabled",
	// The deprecated table decodes into the SAME struct, so every field added to
	// FullSelfPrompting surfaces under both spellings. These two are reachable
	// only by hand-editing the legacy table, which Load then migrates wholesale
	// onto the canonical section — there is nothing to offer an operator here.
	"escalations.full_self_prompting.honour_limits":         "deprecated alias for full_self_prompting.honour_limits",
	"escalations.full_self_prompting.accept_generated_task": "deprecated alias for full_self_prompting.accept_generated_task",
}

// tomlScalarKeys reports every key BurntSushi/toml would accept as a SCALAR
// for rt, and separately every exported field carrying no toml tag.
//
// It models the DECODER's field resolution, not a convenient approximation of
// it, because anything the decoder accepts but this walk misses is a config key
// that can be set in config.toml while `hap config set` rejects it — the exact
// drift TestEveryConfigKeyIsRegistered exists to prevent. Verified against
// BurntSushi/toml v1.6.0:
//
//   - An UNTAGGED exported field is accepted under its Go field name (and
//     case-folded, so `untagged = "x"` reaches `Untagged`). Reported separately
//     rather than keyed, because every field in this config carries an explicit
//     tag and an untagged one would produce a MixedCaps key unlike any other —
//     the fix is to add the tag, not to register the odd key.
//   - An ANONYMOUS embedded struct has its fields PROMOTED to the parent level,
//     so it recurses with the parent's prefix rather than adding a segment.
//   - A type implementing encoding.TextUnmarshaler or toml.Unmarshaler is
//     decoded as a SCALAR even though its Kind is Struct. Recursing into it
//     would find no tagged fields and silently record nothing.
//
// Slices, maps and interfaces are structured data that one key=value assignment
// cannot express, so they are reported SEPARATELY (as lists) rather than as
// scalars: they are not `config set` keys, but they are still config an
// operator must be able to edit, and TestEveryConfigListHasACLICommand holds
// each of them to a verb of its own.
func tomlScalarKeys(rt reflect.Type) (scalars, lists, untagged []string) {
	textUnmarshaler := reflect.TypeOf((*encoding.TextUnmarshaler)(nil)).Elem()
	tomlUnmarshaler := reflect.TypeOf((*toml.Unmarshaler)(nil)).Elem()
	decodesAsScalar := func(ft reflect.Type) bool {
		for _, iface := range []reflect.Type{textUnmarshaler, tomlUnmarshaler} {
			if ft.Implements(iface) || reflect.PointerTo(ft).Implements(iface) {
				return true
			}
		}
		return false
	}

	var walk func(rt reflect.Type, prefix string)
	walk = func(rt reflect.Type, prefix string) {
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			// Unexported fields are unreachable — EXCEPT an anonymous one, whose
			// exported inner fields the decoder still promotes even when the
			// embedded TYPE itself is unexported (reflect sets PkgPath there).
			if f.PkgPath != "" && !f.Anonymous {
				continue
			}
			name, _, _ := strings.Cut(f.Tag.Get("toml"), ",")
			if name == "-" {
				continue
			}

			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}

			// Anonymous embedding promotes the inner fields to this level.
			if f.Anonymous && name == "" && ft.Kind() == reflect.Struct && !decodesAsScalar(ft) {
				walk(ft, prefix)
				continue
			}
			if name == "" {
				untagged = append(untagged, prefix+"."+f.Name)
				continue
			}

			key := name
			if prefix != "" {
				key = prefix + "." + name
			}
			switch {
			case decodesAsScalar(ft):
				scalars = append(scalars, key)
			case ft.Kind() == reflect.Slice, ft.Kind() == reflect.Map, ft.Kind() == reflect.Interface:
				lists = append(lists, key)
			case ft.Kind() == reflect.Struct:
				walk(ft, key)
			default:
				scalars = append(scalars, key)
			}
		}
	}
	walk(rt, "")
	return scalars, lists, untagged
}

// TestEveryConfigKeyIsRegistered keeps `hap config set` in sync with
// config.toml BY CONSTRUCTION rather than by discipline.
//
// The parity tests above only compare the registry against a hand-written
// sample map, so a key added to config.Config and forgotten in the registry was
// invisible to all of them — which is exactly how embedding.bm25_highbar_score
// shipped settable in config.toml but rejected by `hap config set`.
//
// TUI visibility is NOT what this checks: an advanced key belongs in the
// registry with TUIHidden so the CLI reaches it while the TUI stays readable.
func TestEveryConfigKeyIsRegistered(t *testing.T) {
	registered := make(map[string]bool, len(frontend.ConfigFieldKeys))
	for _, key := range frontend.ConfigFieldKeys {
		registered[key] = true
	}

	scalars, _, untagged := tomlScalarKeys(reflect.TypeOf(config.Config{}))

	for _, key := range untagged {
		t.Errorf("config field %s has no toml tag, so the decoder accepts it under its "+
			"Go field NAME — a MixedCaps key unlike every other one. Add an explicit "+
			"toml tag.", strings.TrimPrefix(key, "."))
	}

	present := map[string]bool{}
	for _, key := range scalars {
		present[key] = true
		if registered[key] {
			continue
		}
		if _, exempt := configKeysExemptFromRegistry[key]; exempt {
			continue
		}
		t.Errorf("config key %q is settable in config.toml but missing from "+
			"frontend.ConfigFields — `hap config set %s` rejects it as an unknown "+
			"field. Register it (add TUIHidden if it is too advanced for the TUI), "+
			"or add it to configKeysExemptFromRegistry with a reason.", key, key)
	}

	// The exemption list must not rot: an entry naming a key the struct no
	// longer has is a stale exemption that would silently cover a future key
	// reusing that name.
	for key, why := range configKeysExemptFromRegistry {
		if !present[key] {
			t.Errorf("exemption for %q (%s) names a key config.Config no longer has — "+
				"drop the exemption", key, why)
		}
	}
}

// TestNewConfigFieldsRoundTrip covers what the registry parity test cannot:
// parity only asserts SetField ACCEPTS a sample, so a case that returns nil
// without assigning anything passes it. These keys are checked by reading the
// value back, plus the rejections that make the validation meaningful.
func TestNewConfigFieldsRoundTrip(t *testing.T) {
	app, _ := testApp(t)
	ctx := context.Background()

	t.Run("bm25_highbar_score persists and is bounded", func(t *testing.T) {
		if _, err := app.SetField(ctx, "embedding.bm25_highbar_score", "0.80"); err != nil {
			t.Fatalf("SetField rejected a valid value: %v", err)
		}
		cfg, err := app.Config()
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.Embedding.BM25HighBarScore; got != 0.80 {
			t.Errorf("BM25HighBarScore = %v, want 0.80 — SetField accepted the value but did not store it", got)
		}
		for _, bad := range []string{"0", "1.5", "-0.2", "abc", ""} {
			if _, err := app.SetField(ctx, "embedding.bm25_highbar_score", bad); err == nil {
				t.Errorf("SetField accepted %q; the bar must stay within (0,1]", bad)
			}
		}
		// A value below bm25_min_score is deliberately ALLOWED: the daemon
		// ignores it rather than letting it loosen the fallback, and rejecting
		// it here would make the two keys order-dependent to set.
		if _, err := app.SetField(ctx, "embedding.bm25_highbar_score", "0.10"); err != nil {
			t.Errorf("a high bar below bm25_min_score must be storable, got %v", err)
		}
	})

	t.Run("run_in_agent_cwd persists an explicit false", func(t *testing.T) {
		// A POINTER field is exactly the shape where "SetField accepted the
		// value" and "SetField stored it" come apart — a nil left in place, or
		// a write through a pointer the loaded Config shares, both look fine to
		// the registry-parity guard.
		if _, err := app.SetField(ctx, "llm.run_in_agent_cwd", "false"); err != nil {
			t.Fatalf("SetField rejected a valid bool: %v", err)
		}
		cfg, err := app.Config()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.LLM.RunInAgentCwd == nil || *cfg.LLM.RunInAgentCwd {
			t.Errorf("RunInAgentCwd = %v — SetField accepted false but did not store it", cfg.LLM.RunInAgentCwd)
		}
		if cfg.RunLLMInAgentCwd() {
			t.Error("the accessor must report false once an explicit false is stored")
		}
		// And back on again, so the default is reachable after opting out.
		if _, err := app.SetField(ctx, "llm.run_in_agent_cwd", "true"); err != nil {
			t.Fatalf("SetField rejected true: %v", err)
		}
		if cfg, err = app.Config(); err != nil {
			t.Fatal(err)
		}
		if !cfg.RunLLMInAgentCwd() {
			t.Error("RunInAgentCwd must be settable back to true")
		}
		for _, bad := range []string{"yes", "", "1.5", "maybe"} {
			if _, err := app.SetField(ctx, "llm.run_in_agent_cwd", bad); err == nil {
				t.Errorf("SetField accepted %q; only a bool is valid", bad)
			}
		}
	})

	t.Run("learn_from_user keys persist, validate, and clear", func(t *testing.T) {
		if _, err := app.SetField(ctx, "llm.learn_from_user_command", `claude -p "record the lesson"`); err != nil {
			t.Fatalf("SetField rejected a valid argv: %v", err)
		}
		cfg, err := app.Config()
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.LLM.LearnFromUserCommand; len(got) != 3 || got[0] != "claude" || got[2] != "record the lesson" {
			t.Errorf("LearnFromUserCommand = %q — SetField accepted the value but did not store it as argv", got)
		}
		// Empty disables the feature; that is a setting, not an error.
		if _, err := app.SetField(ctx, "llm.learn_from_user_command", ""); err != nil {
			t.Errorf("an empty command must disable the feature, got %v", err)
		}
		if cfg, _ = app.Config(); len(cfg.LLM.LearnFromUserCommand) != 0 {
			t.Errorf("empty command did not clear: %q", cfg.LLM.LearnFromUserCommand)
		}
		// An unbalanced quote is a real parse error, not a silent single arg.
		if _, err := app.SetField(ctx, "llm.learn_from_user_command", `claude -p "unterminated`); err == nil {
			t.Error("SetField accepted an unbalanced quote")
		}

		if _, err := app.SetField(ctx, "llm.learn_from_user_timeout_seconds", "90"); err != nil {
			t.Fatalf("SetField rejected a valid timeout: %v", err)
		}
		if cfg, _ = app.Config(); cfg.LLM.LearnFromUserTimeoutSeconds != 90 {
			t.Errorf("LearnFromUserTimeoutSeconds = %d, want 90", cfg.LLM.LearnFromUserTimeoutSeconds)
		}
		// 0 is legal and means "inherit timeout_seconds"; negatives and
		// non-numbers are not.
		if _, err := app.SetField(ctx, "llm.learn_from_user_timeout_seconds", "0"); err != nil {
			t.Errorf("0 must be accepted (inherits timeout_seconds), got %v", err)
		}
		for _, bad := range []string{"-1", "abc", ""} {
			if _, err := app.SetField(ctx, "llm.learn_from_user_timeout_seconds", bad); err == nil {
				t.Errorf("SetField accepted %q for a non-negative integer field", bad)
			}
		}

		if _, err := app.SetField(ctx, "llm.learn_from_user_command_env_file", "/etc/hap/learn.env"); err != nil {
			t.Fatalf("SetField rejected a valid env path: %v", err)
		}
		if cfg, _ = app.Config(); cfg.LLM.LearnFromUserEnvFile != "/etc/hap/learn.env" {
			t.Errorf("LearnFromUserEnvFile = %q, want the path", cfg.LLM.LearnFromUserEnvFile)
		}
		if _, err := app.SetField(ctx, "llm.learn_from_user_command_env_file", ""); err != nil {
			t.Errorf("an empty env path must clear it, got %v", err)
		}
	})

	t.Run("palette roles persist, validate, and clear", func(t *testing.T) {
		roles := map[string]func(config.Config) string{
			"tui.palette.title":   func(c config.Config) string { return c.TUI.Palette.Title },
			"tui.palette.section": func(c config.Config) string { return c.TUI.Palette.Section },
			"tui.palette.error":   func(c config.Config) string { return c.TUI.Palette.Error },
			"tui.palette.ok":      func(c config.Config) string { return c.TUI.Palette.OK },
			"tui.palette.paused":  func(c config.Config) string { return c.TUI.Palette.Paused },
			"tui.palette.running": func(c config.Config) string { return c.TUI.Palette.Running },
			"tui.palette.warn":    func(c config.Config) string { return c.TUI.Palette.Warn },
			"tui.palette.help":    func(c config.Config) string { return c.TUI.Palette.Help },
		}
		for key, read := range roles {
			if _, err := app.SetField(ctx, key, "205"); err != nil {
				t.Fatalf("SetField(%s, 205): %v", key, err)
			}
			cfg, err := app.Config()
			if err != nil {
				t.Fatal(err)
			}
			if got := read(cfg); got != "205" {
				t.Errorf("%s = %q after set, want \"205\" — the case is wired to the wrong field or does not assign", key, got)
			}
			// "" clears the role back to the theme; that is a setting, not an error.
			if _, err := app.SetField(ctx, key, ""); err != nil {
				t.Errorf("SetField(%s, \"\") must clear the role, got %v", key, err)
			}
			cleared, err := app.Config()
			if err != nil {
				t.Fatal(err)
			}
			if got := read(cleared); got != "" {
				t.Errorf("%s = %q after clearing, want empty", key, got)
			}
		}

		// Accepted forms, on one representative role.
		for _, ok := range []string{"0", "255", "#abc", "#a1b2c3", "#ABCDEF"} {
			if _, err := app.SetField(ctx, "tui.palette.title", ok); err != nil {
				t.Errorf("SetField(tui.palette.title, %q) rejected a valid color: %v", ok, err)
			}
		}
		// Rejected: lipgloss resolves each of these to NO color (or an
		// out-of-spec SGR) silently, and a TUIHidden key has no other feedback.
		for _, bad := range []string{"purple", "300", "-1", "#ab", "#abcd", "#gggggg", "1.5"} {
			if _, err := app.SetField(ctx, "tui.palette.title", bad); err == nil {
				t.Errorf("SetField(tui.palette.title, %q) was accepted; it renders as no color at all", bad)
			}
		}
	})
}

// tomlProbeCustom decodes from a TOML string even though its Kind is Struct.
type tomlProbeCustom struct{ raw string }

func (c *tomlProbeCustom) UnmarshalText(b []byte) error { c.raw = string(b); return nil }

type tomlProbeEmbedded struct {
	Promoted string `toml:"promoted"`
}

type tomlProbeNested struct {
	Inner string `toml:"inner"`
}

type tomlProbeConfig struct {
	Tagged   string `toml:"tagged"`
	Untagged string // decoder accepts this under the Go field name
	tomlProbeEmbedded
	Custom   tomlProbeCustom `toml:"custom"` // scalar despite Kind() == Struct
	Nested   tomlProbeNested `toml:"nested"`
	Listed   []string        `toml:"listed"` // structured: config.toml-only
	Mapped   map[string]int  `toml:"mapped"` // structured: config.toml-only
	Skipped  string          `toml:"-"`      // decoder ignores
	unexpvar string          `toml:"unexp"`  //nolint:unused // decoder cannot reach it
}

// TestTOMLScalarKeysModelsTheDecoder pins the field-resolution rules
// TestEveryConfigKeyIsRegistered depends on. Each shape here is one the decoder
// accepts but a naive `toml`-tag walk misses, so a regression in this helper
// would silently reopen the config/CLI drift that guard exists to prevent.
//
// The premise is verified against the real decoder, not assumed: the subtest
// below round-trips the same struct through toml.Unmarshal, so if
// BurntSushi/toml ever changes these rules this test fails rather than pinning
// a model of a decoder that no longer exists.
func TestTOMLScalarKeysModelsTheDecoder(t *testing.T) {
	scalars, lists, untagged := tomlScalarKeys(reflect.TypeOf(tomlProbeConfig{}))

	got := map[string]bool{}
	for _, k := range scalars {
		got[k] = true
	}
	// A slice or map is not a `config set` key, but it IS a config key: it must
	// come back as a LIST, not vanish, or TestEveryConfigListHasACLICommand
	// would silently stop noticing a new section with no CLI command.
	gotList := map[string]bool{}
	for _, k := range lists {
		gotList[k] = true
	}
	for _, want := range []string{"listed", "mapped"} {
		if !gotList[want] {
			t.Errorf("tomlScalarKeys dropped %q instead of reporting it as a list — a config "+
				"array/map with no CLI command would then go unnoticed", want)
		}
	}
	for _, want := range []string{
		"tagged",
		"promoted", // anonymous embedding promotes to the PARENT level
		"custom",   // TextUnmarshaler: scalar, not a table to recurse into
		"nested.inner",
	} {
		if !got[want] {
			t.Errorf("tomlScalarKeys missed %q — the decoder accepts it, so a real config "+
				"field of this shape would be settable in config.toml yet unregistered", want)
		}
	}
	for _, notWant := range []string{"listed", "mapped", "unexp", "-", "custom.raw", "tomlProbeEmbedded.promoted"} {
		if got[notWant] {
			t.Errorf("tomlScalarKeys reported %q as a settable scalar; it is not one", notWant)
		}
	}
	if len(untagged) != 1 || !strings.HasSuffix(untagged[0], "Untagged") {
		t.Errorf("untagged = %v, want exactly the one untagged exported field — an "+
			"untagged field is reachable from config.toml under its Go name and must "+
			"be reported, not skipped", untagged)
	}

	t.Run("the decoder really does accept these shapes", func(t *testing.T) {
		var c tomlProbeConfig
		if err := toml.Unmarshal([]byte(
			"tagged=\"a\"\nUntagged=\"b\"\npromoted=\"c\"\ncustom=\"d\"\n[nested]\ninner=\"e\"\n"), &c); err != nil {
			t.Fatal(err)
		}
		if c.Tagged != "a" || c.Untagged != "b" || c.Promoted != "c" ||
			c.Custom.raw != "d" || c.Nested.Inner != "e" {
			t.Errorf("decoder no longer resolves fields the way tomlScalarKeys models: %+v", c)
		}
	})
}

// TestSignaturesLoadsConfigOncePerCall pins the fix for the TUI's runaway CPU
// use. App.Config re-reads and re-parses config.toml on every call (~9ms of
// TOML decoding, deliberately uncached so an operator edit takes effect on the
// next read). Signatures used to resolve the confirmation weight INSIDE its
// per-signature loop, making the listing O(signatures) file reads: with ~80
// learned rules that is ~700ms of pure CPU, and the TUI calls Signatures on a
// 2s refresh, so `hap tui` sat at ~35% of a core for as long as it was open.
// The weight cannot change mid-listing, so one load per call is the ceiling.
func TestSignaturesLoadsConfigOncePerCall(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	now := time.Now()

	const sigCount = 12
	for i := 0; i < sigCount; i++ {
		sig := fmt.Sprintf("approval:%02d", i)
		if err := st.UpsertSignature(ctx, domain.SignatureState{
			Signature: sig, SituationType: domain.SituationApproval, AgentType: "claude",
			Mode: domain.ModeShadow, UpdatedAt: now.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.RecordDecision(ctx, domain.DecisionRecord{Signature: sig,
			SituationType: domain.SituationApproval, AgentType: "claude",
			ChosenAction: "1", Source: domain.SourceOperator, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}

	var loads int
	app.LoadConfig = func(path string) (config.Config, error) {
		loads++
		return config.Load(path)
	}

	rows, err := app.Signatures(ctx, domain.SignatureFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != sigCount {
		t.Fatalf("got %d rows, want %d", len(rows), sigCount)
	}
	// Exactly one, not "at most one": zero would mean a refactor had dropped the
	// config read altogether and hardcoded the default weight, which passes a
	// `> 1` assertion while silently ignoring learning.confirmation_weight.
	// TestSignaturesAppliesConfiguredConfirmationWeight pins the other half.
	if loads != 1 {
		t.Errorf("Signatures over %d rules loaded config %d times, want exactly 1 — "+
			"the confirmation weight must be resolved once, outside the per-signature loop",
			sigCount, loads)
	}
}

// TestSignaturesAppliesConfiguredConfirmationWeight is the companion to the
// load-count test above: hoisting the config read out of the loop must not
// become "stop reading config at all". The operator's
// learning.confirmation_weight has to reach domain.LiveConfidence, so a rule
// whose history mixes one operator confirmation with one non-operator auto-send
// scores differently under a heavy weight than under no boost at all.
func TestSignaturesAppliesConfiguredConfirmationWeight(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	now := time.Now()

	const sig = "approval:weighted"
	if err := st.UpsertSignature(ctx, domain.SignatureState{
		Signature: sig, SituationType: domain.SituationApproval, AgentType: "claude",
		Mode: domain.ModeShadow, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	// Two competing actions. Only the operator one is boosted by the weight
	// (Confidence boosts Source==SourceOperator && !IsCorrection), so the
	// agreement ratio moves with it. A single-action history would score 1.0
	// under every weight and prove nothing.
	if _, err := st.RecordDecision(ctx, domain.DecisionRecord{Signature: sig,
		SituationType: domain.SituationApproval, AgentType: "claude",
		ChosenAction: "1", Source: domain.SourceOperator, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordDecision(ctx, domain.DecisionRecord{Signature: sig,
		SituationType: domain.SituationApproval, AgentType: "claude",
		ChosenAction: "2", Source: domain.SourceRule, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}

	scoreAt := func(weight float64) float64 {
		t.Helper()
		cfg := config.Default()
		cfg.Learning.ConfirmationWeight = weight
		if err := config.Save(app.ConfigPath, cfg); err != nil {
			t.Fatal(err)
		}
		rows, err := app.Signatures(ctx, domain.SignatureFilter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Fatalf("got %d rows, want 1", len(rows))
		}
		return rows[0].Confidence
	}

	unboosted := scoreAt(1) // 1 == no boost (Confidence clamps anything below 1)
	boosted := scoreAt(9)
	if boosted <= unboosted {
		t.Errorf("confirmation_weight is not reaching LiveConfidence: score was %.3f at weight 1 "+
			"and %.3f at weight 9, want the operator-confirmed action to score higher when boosted",
			unboosted, boosted)
	}
}

// noBatchStore hides the optional bulk decision reads. It embeds the INTERFACE,
// not the concrete store, so the batch methods are not promoted and the type
// assertion in App.Signatures fails — which is exactly the shape of any fake
// that does not implement ports.BatchDecisionReader.
type noBatchStore struct{ ports.FrontendStore }

// TestSignaturesBatchAndFallbackAgree pins the optional-capability contract.
// App.Signatures takes two bulk queries when the store offers them and two
// queries PER RULE when it does not; the fast path is only legitimate while
// both produce identical rows. A drift here would show the operator different
// confidence depending on which store implementation they happened to have.
func TestSignaturesBatchAndFallbackAgree(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	now := time.Now()

	// Mixed history so the confidence scores are not all trivially 1.0: two
	// competing actions, an operator confirmation among them, and one rule with
	// no decisions at all.
	for i, sig := range []string{"approval:a", "choice:b", "idle:c"} {
		if err := st.UpsertSignature(ctx, domain.SignatureState{
			Signature: sig, SituationType: domain.SituationApproval, AgentType: "claude",
			Mode: domain.ModeShadow, UpdatedAt: now.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
		if sig == "idle:c" {
			continue
		}
		for j := 0; j < 5+i; j++ {
			action, source := "1", domain.SourceOperator
			if j%3 == 0 {
				action, source = "2", domain.SourceRule
			}
			if _, err := st.RecordDecision(ctx, domain.DecisionRecord{Signature: sig,
				SituationType: domain.SituationApproval, AgentType: "claude",
				ChosenAction: action, Source: source, CreatedAt: now}); err != nil {
				t.Fatal(err)
			}
		}
	}

	if _, ok := app.Store.(ports.BatchDecisionReader); !ok {
		t.Fatal("the real store must implement ports.BatchDecisionReader, or this test proves nothing")
	}
	batched, err := app.Signatures(ctx, domain.SignatureFilter{})
	if err != nil {
		t.Fatal(err)
	}

	app.Store = noBatchStore{app.Store}
	if _, ok := app.Store.(ports.BatchDecisionReader); ok {
		t.Fatal("noBatchStore still exposes the batch reads; the fallback path is not being exercised")
	}
	fallback, err := app.Signatures(ctx, domain.SignatureFilter{})
	if err != nil {
		t.Fatal(err)
	}

	if len(batched) != len(fallback) {
		t.Fatalf("batched returned %d rows, fallback %d", len(batched), len(fallback))
	}
	for i := range batched {
		b, f := batched[i], fallback[i]
		if b.Signature != f.Signature || b.Confidence != f.Confidence ||
			b.TopAction != f.TopAction || b.Decisions != f.Decisions ||
			b.TotalDecisions != f.TotalDecisions {
			t.Errorf("row %d differs\n batched:  %+v\n fallback: %+v", i, b, f)
		}
	}
}

// TestAddTaskCreatesAMissingConfiguredList: `add` is the one op that creates
// content, and for a REMOTE list it is the only way to create one — there is
// no file for the operator to touch, and hap's own hints (the TUI's add
// refusal, the remote task-management hints) send them to this command. A
// fresh gist-backed source was a deadlock before this: the list appeared only
// on the daemon's first hand-out, which cannot happen until the list holds a
// task.
func TestAddTaskCreatesAMissingConfiguredList(t *testing.T) {
	app, _ := testApp(t)
	// A configured source in a directory that does not exist either, so the
	// create has to build the whole path the way the bootstrap does.
	path := filepath.Join(t.TempDir(), "nested", "tasks.md")
	if err := app.AddTaskSource(context.Background(), "brave-otter", "", path, ""); err != nil {
		t.Fatal(err)
	}

	items, idx, err := app.AddTask("brave-otter", "", "write the migration test")
	if err != nil {
		t.Fatalf("add to a not-yet-created configured list: %v", err)
	}
	if idx != 1 || len(items) != 1 || items[0].Text != "write the migration test" {
		t.Fatalf("add returned idx=%d items=%+v, want the one new task", idx, items)
	}
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("the list should exist now: %v", rerr)
	}
	// Created with the same header shape the generated-task bootstrap writes,
	// so a list hap made is recognizable however it was made.
	if !strings.HasPrefix(string(data), "# Tasks for brave-otter\n") {
		t.Errorf("created list = %q, want the titled header", data)
	}
	if !strings.Contains(string(data), "- [ ] write the migration test") {
		t.Errorf("created list = %q, want the added task", data)
	}

	// A second add must APPEND, never re-create over the first.
	if _, idx2, err := app.AddTask("brave-otter", "", "second"); err != nil || idx2 != 2 {
		t.Fatalf("second add: idx=%d err=%v, want it appended as #2", idx2, err)
	}
}

// TestAddTaskPathTargetStillRefusesAMissingFile: the create is scoped to a
// CONFIGURED source. --path is a path the caller typed, so a typo must fail
// loudly rather than silently minting an empty checklist somewhere else
// (ports.EnsureCreator's standing rule).
func TestAddTaskPathTargetStillRefusesAMissingFile(t *testing.T) {
	app, _ := testApp(t)
	typo := filepath.Join(t.TempDir(), "tsaks.md")
	if _, _, err := app.AddTask("", typo, "x"); err == nil {
		t.Fatal("--path to a missing file must refuse")
	}
	if _, err := os.Stat(typo); !os.IsNotExist(err) {
		t.Errorf("a refused --path add must create nothing, stat err = %v", err)
	}
}

// TestConfigWriteSucceedsWithNoDaemonListening: a nudge only asks a RUNNING
// daemon to re-read what was already persisted, so its failure must never be
// the write's failure. It was: `hap config …` before the daemon's first start
// — the ordinary first-run order — exited 1 printing only "daemon nudge
// failed" while the source had in fact been added, so an operator could
// re-run it and double-add.
func TestConfigWriteSucceedsWithNoDaemonListening(t *testing.T) {
	app, _ := testApp(t)
	// A control path that nothing is listening on, which is exactly what a
	// stopped daemon leaves behind.
	app.ControlPath = filepath.Join(t.TempDir(), "control.sock")

	path := filepath.Join(t.TempDir(), "tasks.md")
	if err := os.WriteFile(path, []byte("- [ ] a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := app.AddTaskSource(context.Background(), "brave-otter", "", path, ""); err != nil {
		t.Fatalf("a failed nudge must not fail the config write: %v", err)
	}
	cfg, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.TaskSources) != 1 {
		t.Fatalf("the source should have been saved, got %d", len(cfg.TaskSources))
	}
}

// TestTaskGroupsDerivedSourceResolvesByItsNamedSelector: a derived source
// scoped to an agent NAME addresses exactly one list whether or not that agent
// is running — the name is all the derived file name is built from. Before
// this, an agent's list vanished from the Tasks tab the moment its pane
// closed, while `hap task <name> list` kept printing it.
func TestTaskGroupsDerivedSourceResolvesByItsNamedSelector(t *testing.T) {
	app, _ := testApp(t)
	cfg := derivedGistCfg(t, "brave-otter")

	// No live agents at all — the agent's pane has closed — but hap has seen
	// it before, so the registry still names it. That registry entry is the
	// evidence the selector is an agent NAME rather than an agent type.
	groups := app.TaskGroups(cfg, frontend.Status{
		AgentsKnown:     true,
		AgentNames:      map[string]string{"t9": "brave-otter"},
		AgentNamesKnown: true,
	})
	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1", len(groups))
	}
	if want := "gist://3f2a1b9c/brave-otter.md"; groups[0].Locator != want {
		t.Fatalf("Locator=%q Err=%q, want the selector's own list %q",
			groups[0].Locator, groups[0].Err, want)
	}
	if strings.Contains(groups[0].Err, derivedTemplateNote) {
		t.Errorf("Err=%q still shows the per-agent note for a named selector", groups[0].Err)
	}
}

// TestTaskGroupsDerivedSourceTypeSelectorStaysUnresolvedWithNothingLive: a
// selector is matched against an agent's id, TYPE or name, and herdr agent
// types are free-form strings, so with nothing live hap cannot tell a type
// from a name by inspection. Resolving a type to "<type>.md" would show — and
// let an operator fill — a list no hand-out ever reads, so the name registry
// has to say the selector is an agent before it resolves.
func TestTaskGroupsDerivedSourceTypeSelectorStaysUnresolvedWithNothingLive(t *testing.T) {
	app, _ := testApp(t)
	groups := app.TaskGroups(derivedGistCfg(t, "claude"), frontend.Status{
		AgentsKnown: true,
		// Registered agents exist, but none is named "claude" — and the
		// registry was actually READ, which is what makes that absence
		// evidence rather than ignorance.
		AgentNames:      map[string]string{"t1": "brave-otter"},
		AgentNamesKnown: true,
	})
	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1", len(groups))
	}
	if groups[0].Locator != "" {
		t.Fatalf("Locator=%q, want none — a bare agent TYPE names no single list", groups[0].Locator)
	}
	if !strings.Contains(groups[0].Err, derivedTemplateNote) {
		t.Fatalf("Err=%q, want the per-agent note", groups[0].Err)
	}
}

// TestTaskGroupsDerivedSourceWithSeveralLiveMatchesStaysUnresolved: the
// name-selector fallback must never fire for a selector several agents share
// (a TYPE, or a workspace scope). Those agents genuinely have one list each,
// and picking one would show — and let the operator mutate — another agent's
// work.
func TestTaskGroupsDerivedSourceWithSeveralLiveMatchesStaysUnresolved(t *testing.T) {
	app, _ := testApp(t)
	cfg := derivedGistCfg(t, "claude") // an agent TYPE, matching both agents
	st := frontend.Status{
		AgentsKnown: true,
		MonitoredAgents: []domain.AgentTransition{
			{AgentID: "t1", AgentType: "claude", WorkspaceID: "w1"},
			{AgentID: "t2", AgentType: "claude", WorkspaceID: "w1"},
		},
		AgentNames: map[string]string{"t1": "brave-otter", "t2": "calm-badger"},
	}
	groups := app.TaskGroups(cfg, st)
	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1", len(groups))
	}
	if !strings.Contains(groups[0].Err, derivedTemplateNote) {
		t.Fatalf("Err=%q, want the per-agent note", groups[0].Err)
	}
	if groups[0].Locator != "" {
		t.Fatalf("Locator=%q, want none — a shared selector must not pick a list", groups[0].Locator)
	}
}

// TestTaskSourceLocationNamesTheProvider: every surface listing sources must
// render WHERE the list lives through this, never Source.Path — which under a
// remote provider is a bare file name, and for a derived source is empty, so
// a raw Path renders as a blank column that reads as "unconfigured" for the
// most ordinary gist setup there is.
func TestTaskSourceLocationNamesTheProvider(t *testing.T) {
	gist := derivedGistCfg(t, "brave-otter")
	if got := frontend.TaskSourceLocation(gist, gist.TaskSources[0]); !strings.Contains(got, "github_gist") {
		t.Errorf("derived gist source rendered %q, want the provider named", got)
	}
	explicit := config.TaskSource{Agent: "brave-otter", Path: "shared.md"}
	gist.TaskSources = []config.TaskSource{explicit}
	got := frontend.TaskSourceLocation(gist, explicit)
	if !strings.Contains(got, "shared.md") || !strings.Contains(got, "github_gist") {
		t.Errorf("explicit gist source rendered %q, want the file and the provider", got)
	}
	// A local source still reads as the plain path the operator wrote.
	local := config.Config{TaskSources: []config.TaskSource{{Agent: "x", Path: "/docs/tasks.md"}}}
	if got := frontend.TaskSourceLocation(local, local.TaskSources[0]); got != "/docs/tasks.md" {
		t.Errorf("local source rendered %q, want the bare path", got)
	}
}

// TestSetFieldReportsWhetherADaemonTookTheReload: the surfaces that print
// "daemon reloaded" must not say so when nothing was listening. The write
// still succeeds — a stopped daemon reads config at its next start — so the
// flag is for WORDING only and must never be turned into a failure.
func TestSetFieldReportsWhetherADaemonTookTheReload(t *testing.T) {
	app, _ := testApp(t)
	app.ControlPath = filepath.Join(t.TempDir(), "control.sock") // nothing listening

	reloaded, err := app.SetField(context.Background(), "learning.graduation_n", "4")
	if err != nil {
		t.Fatalf("a set with no daemon must still succeed: %v", err)
	}
	if reloaded {
		t.Error("reloaded = true with nothing listening — the message would claim a reload that never happened")
	}
	cfg, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Learning.GraduationN != 4 {
		t.Errorf("graduation_n = %d, want the value saved despite no daemon", cfg.Learning.GraduationN)
	}
}

// TestAddTaskCreatesFromTheTUIsLocatorShapedCall: the TUI adds by LOCATOR
// (AddTask("", <locator>, text)) because its rows carry one, so a create gated
// on the argument shape (agent != "") would have worked from the CLI only —
// while the TUI's own refusal message is one of the things that sends an
// operator to this feature. The gate is therefore "this locator belongs to a
// configured source", which both surfaces satisfy.
func TestAddTaskCreatesFromTheTUIsLocatorShapedCall(t *testing.T) {
	app, st := testApp(t)
	// The registry names the agent: for a DERIVED source that is the evidence
	// the selector is an agent rather than an agent type.
	if _, err := st.EnsureAgentName(context.Background(), "w1:p1"); err != nil {
		t.Fatal(err)
	}
	names, err := st.AgentNames(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	name := names["w1:p1"]
	if name == "" {
		t.Fatal("expected a minted agent name")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.md")
	if err := app.AddTaskSource(context.Background(), name, "", path, ""); err != nil {
		t.Fatal(err)
	}
	// No agent argument, the resolved locator as `path` — exactly how the
	// Tasks tab calls it.
	if _, _, err := app.AddTask("", path, "from the TUI"); err != nil {
		t.Fatalf("a locator-shaped add on a configured source must create the list: %v", err)
	}
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("the list should exist now: %v", rerr)
	}
	if !strings.Contains(string(data), "- [ ] from the TUI") {
		t.Errorf("created list = %q, want the added task", data)
	}
}

// TestAddTaskRefusesToCreateADerivedListForAnUnknownSelector: a DERIVED source
// (remote provider, no file name) resolves per AGENT, so creating
// "<selector>.md" for a selector that names no agent hap has ever seen would
// queue work into a list no hand-out reads — silently, which is worse than the
// error it replaces. A selector is matched against an agent's id, type OR
// name, so "claude" is as likely a TYPE as a name.
func TestAddTaskRefusesToCreateADerivedListForAnUnknownSelector(t *testing.T) {
	app, _ := testApp(t)
	cfg := derivedGistCfg(t, "claude") // derived: remote provider, no path
	if err := config.Save(app.ConfigPath, cfg); err != nil {
		t.Fatal(err)
	}
	_, _, err := app.AddTask("claude", "", "should not create claude.md")
	if err == nil {
		t.Fatal("a derived source whose selector names no known agent must refuse")
	}
	// The refusal has to SAY it is a policy, or it reads as a broken list.
	if !strings.Contains(err.Error(), "will not create") {
		t.Errorf("error = %v, want it to explain the refusal", err)
	}
}

// TestAddTaskCreatesForADerivedSourceOfAKnownAgent: the same derived shape,
// once the agent is in the name registry, is exactly the case create-on-demand
// exists for — an agent whose list has never been written.
func TestAddTaskCreatesForADerivedSourceOfAKnownAgent(t *testing.T) {
	app, st := testApp(t)
	if _, err := st.EnsureAgentName(context.Background(), "w1:p1"); err != nil {
		t.Fatal(err)
	}
	names, err := st.AgentNames(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	name := names["w1:p1"]

	cfg := derivedGistCfg(t, name)
	if err := config.Save(app.ConfigPath, cfg); err != nil {
		t.Fatal(err)
	}
	// The gist itself is unreachable here (no credentials), so the add cannot
	// succeed — but it must fail on the STORE, never on hap's own refusal.
	_, _, aerr := app.AddTask(name, "", "seed")
	if aerr != nil && strings.Contains(aerr.Error(), "will not create") {
		t.Errorf("a known agent's derived source must not be refused by policy, got %v", aerr)
	}
}

// TestTaskGroupsResolvesWhenTheNameRegistryCouldNotBeRead: absence is evidence
// only when the registry was actually read. A failed query leaves AgentNames
// nil, and treating that as "no agent has this name" would un-resolve every
// named source at once — the exact symptom this resolution exists to fix. The
// same rule AgentsKnown carries, for the same reason.
func TestTaskGroupsResolvesWhenTheNameRegistryCouldNotBeRead(t *testing.T) {
	app, _ := testApp(t)
	groups := app.TaskGroups(derivedGistCfg(t, "brave-otter"), frontend.Status{
		AgentsKnown: true,
		// AgentNamesKnown false: the registry query failed.
		AgentNames: nil,
	})
	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1", len(groups))
	}
	if groups[0].Locator == "" {
		t.Fatalf("Err=%q, want the source resolved — an unread registry is not proof the agent is unknown", groups[0].Err)
	}
}

// TestTaskCapAppliesToALocatorShapedAdd: the cap lookup and the create gate
// must find the SAME source. They diverged once — the gate grew a fallback for
// the TUI's locator-shaped call and the cap lookup did not — so a TUI add read
// max_tasks as UNCAPPED, silently ignoring a limit the operator had set.
func TestTaskCapAppliesToALocatorShapedAdd(t *testing.T) {
	app, _ := testApp(t)
	path := filepath.Join(t.TempDir(), "tasks.md")
	if err := os.WriteFile(path, []byte("- [ ] one\n- [ ] two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := app.AddTaskSource(context.Background(), "brave-otter", "", path, "",
		frontend.MaxTasks(2)); err != nil {
		t.Fatal(err)
	}
	// Addressed exactly as the Tasks tab does: no agent, the locator as path.
	if _, _, err := app.AddTask("", path, "third"); err == nil {
		t.Fatal("the cap must apply however the list is addressed")
	} else if !strings.Contains(err.Error(), "cap 2") {
		t.Errorf("error = %v, want the cap refusal", err)
	}
}

// TestAddingATaskSourceNeverRenumbersTheExistingOnes: a source's INDEX is a
// public selector — `hap task 2 …`, the `{task_source_index}` a delivered
// prompt tells an agent to use, and the number every `hap config task-source`
// listing and edit takes. So adding one must APPEND: an insert anywhere else
// silently re-points every selector after it, including commands already sent
// to an agent. Both write paths are covered, because both are reachable from
// the operator's surfaces: `AddTaskSource` (CLI `config task-source add` and
// the TUI Config tab) and the generated-task bootstrap (accepting an LLM task
// suggestion, which registers a source as a side effect).
func TestAddingATaskSourceNeverRenumbersTheExistingOnes(t *testing.T) {
	app, _ := testApp(t)
	ctx := context.Background()
	dir := t.TempDir()

	before := []string{"first", "second", "third"}
	for _, name := range before {
		if err := app.AddTaskSource(ctx, name, "", filepath.Join(dir, name+".md"), ""); err != nil {
			t.Fatal(err)
		}
	}
	assertOrder := func(what string, want []string) {
		t.Helper()
		cfg, err := app.Config()
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.TaskSources) != len(want) {
			t.Fatalf("%s: %d sources, want %d", what, len(cfg.TaskSources), len(want))
		}
		for i, name := range want {
			if cfg.TaskSources[i].Agent != name {
				t.Fatalf("%s: index %d is %q, want %q — adding a source renumbered the existing ones",
					what, i, cfg.TaskSources[i].Agent, name)
			}
		}
	}
	assertOrder("baseline", before)

	// 1. The config surface (CLI `config task-source add`, TUI Config tab).
	if err := app.AddTaskSource(ctx, "fourth", "", filepath.Join(dir, "fourth.md"), ""); err != nil {
		t.Fatal(err)
	}
	assertOrder("after config add", append(append([]string{}, before...), "fourth"))

	// 2. An unrelated config write rewrites the whole file; order must survive
	// the round-trip (Load → edit → Save), not just the append.
	if _, err := app.SetField(ctx, "learning.graduation_n", "3"); err != nil {
		t.Fatal(err)
	}
	assertOrder("after an unrelated config write", append(append([]string{}, before...), "fourth"))
}

// TestAcceptingAGeneratedTaskAppendsItsSource: the escalation-accept path
// registers a task source as a side effect, and it is the one an operator
// triggers without naming a source at all — so it is the likeliest place for
// an insert to creep in. It must append like every other surface.
func TestAcceptingAGeneratedTaskAppendsItsSource(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	dir := t.TempDir()

	// Two sources for OTHER agents, whose indexes must not move.
	for _, name := range []string{"first", "second"} {
		if err := app.AddTaskSource(ctx, name, "", filepath.Join(dir, name+".md"), ""); err != nil {
			t.Fatal(err)
		}
	}

	// An idle escalation carrying an LLM task suggestion, for an agent with no
	// source of its own — accepting it bootstraps one.
	app.StateDir = t.TempDir()
	if _, err := st.EnsureAgentName(ctx, "w9:p1"); err != nil {
		t.Fatal(err)
	}
	id, err := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w9:p1", AgentType: "claude", Signature: "sig-gen", Trigger: "t",
		SituationType: domain.SituationIdle, Action: "escalated", Status: "escalated",
		Suggestion: domain.SuggestTaskPrefix + "write the migration test", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// send=false: the source registration is the part under test, and there is
	// no live agent to deliver to.
	if err := app.Confirm(ctx, id, false); err != nil {
		t.Fatalf("confirm generated task: %v", err)
	}

	cfg, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.TaskSources) != 3 {
		t.Fatalf("got %d sources, want the two originals plus the bootstrapped one", len(cfg.TaskSources))
	}
	if cfg.TaskSources[0].Agent != "first" || cfg.TaskSources[1].Agent != "second" {
		t.Fatalf("existing sources moved: %q, %q — accepting a suggestion renumbered them",
			cfg.TaskSources[0].Agent, cfg.TaskSources[1].Agent)
	}
}

// seedAdjustable stores one shadow rule plus `decisions` consistent operator
// confirmations, so LiveConfidence clears the approval threshold and the only
// thing standing between the rule and graduation is its streak.
func seedAdjustable(t *testing.T, st *store.Store, sig string, decisions int) time.Time {
	t.Helper()
	ctx := context.Background()
	now := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := st.UpsertSignature(ctx, domain.SignatureState{
		Signature: sig, SituationType: domain.SituationApproval,
		AgentType: "claude", Mode: domain.ModeShadow, ConsecutiveConfirmations: 0,
		CachedConfidence: 0.4, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < decisions; i++ {
		if _, err := st.RecordDecision(ctx, domain.DecisionRecord{Signature: sig,
			SituationType: domain.SituationApproval, AgentType: "claude",
			ChosenAction: "1", Source: domain.SourceOperator, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	return now
}

func TestAdjustSignatureConfirmationsThroughApp(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	seedAdjustable(t, st, "approval:nudge", 2)

	// graduation_n defaults to 1, so one `+` reaches N; the confirmations above
	// carry live confidence past the 0.70 approval threshold, so it graduates.
	up, err := app.AdjustSignatureConfirmations(ctx, "approval:nu", 1)
	if err != nil {
		t.Fatal(err)
	}
	if up.Signature != "approval:nudge" {
		t.Errorf("resolved sig = %q, want approval:nudge", up.Signature)
	}
	if up.Confirmations != 1 || up.GraduationN != 1 {
		t.Errorf("streak = %d/%d, want 1/1", up.Confirmations, up.GraduationN)
	}
	if !up.Graduated() || up.Mode != domain.ModeAutonomous {
		t.Fatalf("reaching N with confidence %.2f > %.2f must graduate, got %s",
			up.Confidence, up.Threshold, up.Mode)
	}
	got, _ := st.GetSignature(ctx, "approval:nudge")
	if got == nil || got.Mode != domain.ModeAutonomous || got.ConsecutiveConfirmations != 1 {
		t.Errorf("store must hold the graduated rule: %+v", got)
	}

	// `-` is the graded counterpart to reset: back under N demotes, and the
	// decision history survives (unlike ResetSignatureGraduation, which stamps
	// a floor and a fresh 1.0).
	down, err := app.AdjustSignatureConfirmations(ctx, "approval:nu", -1)
	if err != nil {
		t.Fatal(err)
	}
	if !down.Demoted() || down.Mode != domain.ModeShadow || down.Confirmations != 0 {
		t.Fatalf("dropping below N must demote, got mode=%s streak=%d", down.Mode, down.Confirmations)
	}
	got, _ = st.GetSignature(ctx, "approval:nudge")
	if got.CachedConfidence != 0.4 || got.DecisionFloorID != 0 {
		t.Errorf("a nudge must not stamp the floor or the snapshot: conf=%.2f floor=%d",
			got.CachedConfidence, got.DecisionFloorID)
	}
	if decs, _ := st.DecisionsForSignature(ctx, "approval:nudge", 10); len(decs) != 2 {
		t.Errorf("a nudge must keep decision history, got %d", len(decs))
	}

	// Unknown prefix surfaces the resolution error.
	if _, err := app.AdjustSignatureConfirmations(ctx, "nope:xyz", 1); err == nil {
		t.Error("prefix resolution error must surface")
	}
}

func TestAdjustSignatureConfirmationsReportsTheConfidenceBlock(t *testing.T) {
	// A rule with NO post-floor decisions scores 0, so `+` moves the streak past
	// graduation_n and the rule stays shadow. Without ConfidenceBlocked the
	// operator reads that as a dead key, so the outcome has to be reportable.
	app, st := testApp(t)
	ctx := context.Background()
	seedAdjustable(t, st, "approval:cold", 0)

	got, err := app.AdjustSignatureConfirmations(ctx, "approval:co", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Confirmations != 1 {
		t.Fatalf("the streak must still advance, got %d", got.Confirmations)
	}
	if got.Mode != domain.ModeShadow || got.Graduated() {
		t.Fatalf("no confidence must block graduation, got %s", got.Mode)
	}
	if !got.ConfidenceBlocked() {
		t.Errorf("streak %d/%d at confidence %.2f (threshold %.2f) must report as confidence-blocked",
			got.Confirmations, got.GraduationN, got.Confidence, got.Threshold)
	}
	if got.Threshold != 0.70 {
		t.Errorf("approval threshold = %.2f, want the configured 0.70", got.Threshold)
	}
}

func TestAdjustSignatureConfirmationsDoesNotStampUpdatedAt(t *testing.T) {
	// Store.ListSignatures orders by `updated_at DESC` and the Rules tab's
	// cursor is a positional index into that listing, so stamping UpdatedAt
	// here would float the nudged rule to the top on the next refresh and the
	// operator's SECOND `+` press would silently land on a different rule.
	// Do not "fix" the missing stamp.
	app, st := testApp(t)
	ctx := context.Background()
	seeded := seedAdjustable(t, st, "approval:stable", 2)

	if _, err := app.AdjustSignatureConfirmations(ctx, "approval:st", 1); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetSignature(ctx, "approval:stable")
	if got == nil {
		t.Fatal("signature vanished")
	}
	if !got.UpdatedAt.Equal(seeded) {
		t.Errorf("a nudge must not restamp UpdatedAt (it is the list sort key): got %s, want %s",
			got.UpdatedAt, seeded)
	}
}

func TestSignatureAdjustmentSummary(t *testing.T) {
	// Summary is the one sentence both front ends print, so its branches are
	// pinned here rather than reimplemented in each caller's test.
	base := frontend.SignatureAdjustment{
		Signature: "approval:x", Confirmations: 3, GraduationN: 3,
		Confidence: 0.9, Threshold: 0.7,
	}
	graduated := base
	graduated.PreviousMode, graduated.Mode = domain.ModeShadow, domain.ModeAutonomous

	demoted := base
	demoted.Confirmations = 2
	demoted.PreviousMode, demoted.Mode = domain.ModeAutonomous, domain.ModeShadow

	// Scored but under the threshold: name the numbers, since the operator can
	// act on them (confirm a few more times, or lower the threshold).
	scoredBlock := base
	scoredBlock.PreviousMode, scoredBlock.Mode = domain.ModeShadow, domain.ModeShadow
	scoredBlock.Confidence = 0.55

	// Never scored: a 0 means NO evidence, not a measured 0.00, and the fix for
	// the two is different — so it must not print a number at all.
	unscored := scoredBlock
	unscored.Confidence = 0

	// A `-` that leaves a shadow rule still above N is blocked on the same
	// thing as a `+` would be. Summary describes the resulting STATE, so it
	// says so in both directions rather than going quiet on a decrement.
	downBlocked := unscored
	downBlocked.Confirmations = 5

	// Below N and shadow: nothing to explain, just the streak.
	plain := base
	plain.Confirmations = 1
	plain.PreviousMode, plain.Mode = domain.ModeShadow, domain.ModeShadow

	for _, tc := range []struct {
		name string
		in   frontend.SignatureAdjustment
		want []string
		deny []string
	}{
		{"graduated", graduated, []string{"streak 3/3", "graduated to autonomous"}, []string{"confidence"}},
		{"demoted", demoted, []string{"streak 2/3", "demoted to shadow"}, []string{"confidence"}},
		{"scored but blocked", scoredBlock, []string{"streak 3/3", "confidence 0.55 ≤ 0.70 blocks graduation"}, nil},
		{"never scored", unscored, []string{"streak 3/3", "no decisions scored yet"}, []string{"0.00"}},
		{"decrement still blocked", downBlocked, []string{"streak 5/3", "no decisions scored yet"}, []string{"0.00"}},
		{"below N", plain, []string{"streak 1/3", "(shadow)"}, []string{"graduat", "confidence"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.in.Summary()
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("Summary() = %q, want it to contain %q", got, want)
				}
			}
			for _, deny := range tc.deny {
				if strings.Contains(got, deny) {
					t.Errorf("Summary() = %q, must not contain %q", got, deny)
				}
			}
		})
	}
}
