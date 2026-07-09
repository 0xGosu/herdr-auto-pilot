package frontend_test

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/cli"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
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

func TestAddAllowlistPatternValidates(t *testing.T) {
	app, _ := testApp(t)
	ctx := context.Background()
	if err := app.AddAllowlistPattern(ctx, `(?i)restart\s+prod`); err != nil {
		t.Fatal(err)
	}
	cfg, _ := app.Config()
	if len(cfg.Safety.AllowlistPatterns) != 1 {
		t.Error("pattern not persisted")
	}
	if err := app.AddAllowlistPattern(ctx, "([broken"); err == nil {
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
	// Every editable key renders a value.
	for _, key := range frontend.ConfigFieldKeys {
		if key != "llm.command" && frontend.FieldValue(cfg, key) == "" {
			t.Errorf("FieldValue(%s) is empty", key)
		}
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
	app.AddAllowlistPattern(ctx, `(?i)one`)
	app.AddAllowlistPattern(ctx, `(?i)two`)
	app.AddTaskSource(ctx, "builder", "", "/tmp/tasks.md")

	// A stale expectation must refuse to delete (safety-relevant: never
	// silently remove the wrong never-auto pattern).
	if err := app.RemoveAllowlistPattern(ctx, 0, `(?i)two`); err == nil {
		t.Fatal("mismatched expected pattern must refuse removal")
	}
	if err := app.RemoveAllowlistPattern(ctx, 0, `(?i)one`); err != nil {
		t.Fatal(err)
	}
	cfg, _ := app.Config()
	if len(cfg.Safety.AllowlistPatterns) != 1 || cfg.Safety.AllowlistPatterns[0] != `(?i)two` {
		t.Errorf("wrong pattern removed: %v", cfg.Safety.AllowlistPatterns)
	}
	if err := app.RemoveAllowlistPattern(ctx, 5, "x"); err == nil {
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
	if len(cfg.Safety.AllowlistPatterns) != 0 {
		t.Errorf("rules remove failed: %v", cfg.Safety.AllowlistPatterns)
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
