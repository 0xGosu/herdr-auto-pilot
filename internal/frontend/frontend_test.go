package frontend_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/cli"
	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/control"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
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
}

func (f *fakeHerdr) Send(_ context.Context, paneID, input string) error {
	f.panes = append(f.panes, paneID)
	f.inputs = append(f.inputs, input)
	return nil
}

func (f *fakeHerdr) ReadPane(context.Context, string, int) (string, error) {
	return f.pane, f.readErr
}

func (f *fakeHerdr) ListAgents(context.Context) ([]domain.AgentTransition, error) { return nil, nil }

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
	if err != nil || cfg.Thresholds.Approval != 0.93 {
		t.Fatalf("threshold not persisted: %+v %v", cfg.Thresholds, err)
	}
	if err := app.SetThreshold(ctx, "approval", 1.5); err == nil {
		t.Error("out-of-range threshold must be rejected")
	}
	if err := app.SetThreshold(ctx, "bogus", 0.5); err == nil {
		t.Error("unknown situation must be rejected")
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
		{"thresholds.approval", "0.92", false},
		{"thresholds.approval", "1.5", true},
		{"thresholds.approval", "abc", true},
		{"learning.graduation_n", "7", false},
		{"learning.graduation_n", "-1", true},
		{"limits.max_error_retries", "3", false},
		{"llm.timeout_seconds", "90", false},
		{"llm.auto_act", "true", false},
		{"llm.auto_act", "maybe", true},
		{"llm.command", `claude -p "decide for me"`, false},
		{"llm.rewrite_command", `claude -p "rewrite: {text}" --model haiku`, false},
		{"llm.rewrite_command", `claude -p "unbalanced`, true},
		{"llm.rewrite_timeout_seconds", "45", false},
		{"llm.rewrite_timeout_seconds", "zero", true},
		{"llm.rewrite_fallback_template", "Act on: {original_text}", false},
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
	if cfg.Thresholds.Approval != 0.92 || cfg.Learning.GraduationN != 7 ||
		cfg.Limits.MaxErrorRetries != 3 || cfg.LLM.TimeoutSeconds != 90 || !cfg.LLM.AutoAct {
		t.Errorf("persisted config mismatch: %+v", cfg)
	}
	if len(cfg.LLM.Command) != 3 || cfg.LLM.Command[2] != "decide for me" {
		t.Errorf("llm.command quote handling: %v", cfg.LLM.Command)
	}
	if len(cfg.LLM.RewriteCommand) != 5 || cfg.LLM.RewriteCommand[2] != "rewrite: {text}" {
		t.Errorf("llm.rewrite_command quote handling: %v", cfg.LLM.RewriteCommand)
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
		"thresholds.idle":                     "0.70",
		"thresholds.approval":                 "0.85",
		"thresholds.choice":                   "0.85",
		"thresholds.error":                    "0.90",
		"thresholds.inferred_task_bar":        "0.95",
		"learning.graduation_n":               "5",
		"limits.max_consecutive_auto_prompts": "5",
		"limits.max_auto_prompts_per_minute":  "10",
		"limits.max_error_retries":            "2",
		"safety.disable_seed":                 "true",
		"llm.command":                         `claude -p "decide"`,
		"llm.timeout_seconds":                 "60",
		"llm.auto_act":                        "true",
		"llm.pane_excerpt_chars":              "4000",
		"llm.rewrite_command":                 `claude -p "rewrite: {text}"`,
		"llm.rewrite_timeout_seconds":         "45",
		"llm.rewrite_fallback_template":       "Act on: {original_text}",
		"embedding.disabled":                  "false",
		"embedding.model_path":                "/models/custom.gguf",
		"embedding.similarity_threshold":      "0.90",
		"embedding.bm25_min_score":            "0.35",
		"embedding.gpu_layers":                "0",
		"tui.max_content_width":               "140",
		"tui.theme":                           "dark",
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

// TestFieldTUIEditableClassification pins CR-036: free-text fields (argv
// templates, template strings, paths) are read-only in the TUI, everything
// else in the registry is editable, and unknown keys are never editable.
func TestFieldTUIEditableClassification(t *testing.T) {
	readOnly := map[string]bool{
		"llm.command":                   true,
		"llm.rewrite_command":           true,
		"llm.rewrite_fallback_template": true,
		"embedding.model_path":          true,
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
		{"safety.disable_seed", "true", false},
		{"safety.disable_seed", "false", false},
		{"safety.disable_seed", "yes", true},
		{"llm.pane_excerpt_chars", "0", false}, // 0 = restore-default sentinel (fillZeroes)
		{"llm.pane_excerpt_chars", "-5", true},
		{"llm.pane_excerpt_chars", "abc", true},
		{"llm.pane_excerpt_chars", "4000", false},
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
	if err := app.SetField(ctx, "safety.disable_seed", "true"); err != nil {
		t.Fatal(err)
	}
	if cfg, err = app.Config(); err != nil {
		t.Fatal(err)
	}
	if cfg.TUI.MaxContentWidth != 140 {
		t.Errorf("tui.max_content_width = %d, want 140", cfg.TUI.MaxContentWidth)
	}
	if !cfg.Safety.DisableSeed {
		t.Error("safety.disable_seed = false, want true (assignment not persisted)")
	}
	if cfg.LLM.PaneExcerptChars != 4000 {
		t.Errorf("llm.pane_excerpt_chars = %d, want 4000", cfg.LLM.PaneExcerptChars)
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
	if cfg.Thresholds.Choice != 0.9 {
		t.Error("CLI threshold edit must land in shared config")
	}

	// audit + kill-history + rules render without error
	if out := run("audit"); !strings.Contains(out, "escalated") {
		t.Errorf("audit output: %q", out)
	}
	if out := run("kill-history"); !strings.Contains(out, "active") {
		t.Errorf("kill history output: %q", out)
	}
	if out := run("rules", "list"); !strings.Contains(out, "seed") {
		t.Errorf("rules output: %q", out)
	}

	// generic field editor via CLI verb → shared config
	run("config", "set", "learning.graduation_n", "8")
	cfg, _ = app.Config()
	if cfg.Learning.GraduationN != 8 {
		t.Error("config set must land in shared config")
	}
	if out := run("config", "fields"); !strings.Contains(out, "learning.graduation_n") {
		t.Errorf("config fields output: %q", out)
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
	keys []string
}

func (f *fakeKeyHerdr) SendKey(_ context.Context, paneID, key string) error {
	f.keys = append(f.keys, key)
	return nil
}

func TestResolveDigitSeriesDeliversKeystrokes(t *testing.T) {
	// Confirming a multi-tab answer series delivers one digit keystroke per
	// tab (Submit included) — never the series as literal text.
	app, st := testApp(t)
	fake := &fakeKeyHerdr{fakeHerdr: fakeHerdr{
		pane: "←  ☐ Q one  ☐ Q two  ✔ Submit  →\n\nWhich backend?\n❯ 1. sqlite\n  2. postgres\n\nEnter to select · Tab/Arrow keys to navigate · Esc to cancel\n",
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
	// A fixed Left-arrow burst resets the form to question 1 first — the
	// operator may have tabbed around since the escalation was raised.
	want := strings.TrimSpace(strings.Repeat("left ", 10)) + " 1 2 1"
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
