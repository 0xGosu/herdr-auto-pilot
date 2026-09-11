package frontend_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
	"github.com/0xGosu/herdr-auto-pilot/internal/streamlog"
)

func withStream(t *testing.T, app *frontend.App) *streamlog.Log {
	t.Helper()
	log := streamlog.InStateDir(t.TempDir())
	t.Cleanup(func() { _ = log.Close() })
	app.Stream = log
	return log
}

// streamLines returns every event line after seq `after`, without the
// "<seq> <time> " prefix, so assertions read as the payload.
func streamLines(t *testing.T, log *streamlog.Log, after int64) []string {
	t.Helper()
	evs, err := log.Since(context.Background(), after, 1000)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		parts := strings.SplitN(ev.Line(), " ", 3)
		out = append(out, parts[2])
	}
	return out
}

func headOf(t *testing.T, log *streamlog.Log) int64 {
	t.Helper()
	h, err := log.Head(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func wantLines(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("stream events =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

func TestStreamConfigWriteNamesTheChangedKeysOnly(t *testing.T) {
	app, _ := testApp(t)
	log := withStream(t, app)
	ctx := context.Background()
	if _, err := app.SetField(ctx, "full_self_prompting.honour_limits", "true"); err != nil {
		t.Fatal(err)
	}
	wantLines(t, streamLines(t, log, 0), "config.changed keys=full_self_prompting.honour_limits by=operator")

	// A write that changes nothing, and one that fails, say nothing.
	head := headOf(t, log)
	if _, err := app.SetField(ctx, "full_self_prompting.honour_limits", "true"); err != nil {
		t.Fatal(err)
	}
	if _, err := app.SetField(ctx, "full_self_prompting.honour_limits", "maybe"); err == nil {
		t.Fatal("an invalid bool was accepted")
	}
	wantLines(t, streamLines(t, log, head))
}

// Config values can be secrets (env tables, tokens); only key NAMES may reach
// the stream.
func TestStreamConfigEventNeverCarriesAValue(t *testing.T) {
	app, _ := testApp(t)
	log := withStream(t, app)
	if _, err := app.SetField(context.Background(), "llm.command", "claude -p s3cr3t-token"); err != nil {
		t.Fatal(err)
	}
	got := streamLines(t, log, 0)
	wantLines(t, got, "config.changed keys=llm.command by=operator")
	if strings.Contains(strings.Join(got, "\n"), "s3cr3t") {
		t.Fatalf("a config value leaked into the stream: %v", got)
	}
}

func TestStreamTaskSourceAddedAndRemoved(t *testing.T) {
	app, _ := localFSApp(t)
	log := withStream(t, app)
	ctx := context.Background()
	dir := t.TempDir()
	if err := app.AddTaskSource(ctx, "otter", "", dir+"/otter.md", ""); err != nil {
		t.Fatal(err)
	}
	if err := app.AddTaskSource(ctx, "mole", "", dir+"/mole.md", ""); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(app.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	head := headOf(t, log)
	if err := app.RemoveTaskSource(ctx, 0, cfg.TaskSources[0]); err != nil {
		t.Fatal(err)
	}
	all := streamLines(t, log, 0)
	if len(all) < 3 || all[0] != "task_source.added source=0 agent=otter by=operator" ||
		all[1] != "task_source.added source=1 agent=mole by=operator" {
		t.Fatalf("add events = %v", all)
	}
	wantLines(t, streamLines(t, log, head), "task_source.removed source=0 agent=otter by=operator")
}

func TestStreamTaskItemsAndDatabaseList(t *testing.T) {
	app, st := testApp(t)
	log := withStream(t, app)
	if err := os.WriteFile(app.ConfigPath, []byte(
		"[task_source_provider]\nprovider = \"sqlite\"\n\n[[task_sources]]\nagent = \"otter\"\npath = \"otter.md\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	list := "db://" + st.NodeID() + "/otter.md"
	if _, _, err := app.AddTask("otter", "", "first task"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := app.AddTask("otter", "", "second task"); err != nil {
		t.Fatal(err)
	}
	if _, err := app.SetTaskDone("otter", "", 1, true); err != nil {
		t.Fatal(err)
	}
	wantLines(t, streamLines(t, log, 0),
		"tasklist.created list="+list+" by=operator",
		"task.created list="+list+` source=0 index=1 mark=" " by=operator`,
		"task.created list="+list+` source=0 index=2 mark=" " by=operator`,
		"task.updated list="+list+" source=0 index=1 mark=x by=operator",
	)

	head := headOf(t, log)
	if _, err := app.DeleteTaskList(context.Background(), list); err != nil {
		t.Fatal(err)
	}
	wantLines(t, streamLines(t, log, head), "tasklist.deleted list="+list+" by=operator")
}

func TestStreamPauseResumeOnlyOnAChange(t *testing.T) {
	app, _ := testApp(t)
	log := withStream(t, app)
	ctx := context.Background()
	for _, step := range []func(context.Context) (bool, error){app.Pause, app.Pause, app.Resume, app.Resume} {
		if _, err := step(ctx); err != nil {
			t.Fatal(err)
		}
	}
	wantLines(t, streamLines(t, log, 0),
		"pause.on scope=global by=operator",
		"pause.off scope=global by=operator")
}

func seedPendingEscalation(t *testing.T, st *store.Store, agent string) int64 {
	t.Helper()
	id, err := st.AppendAudit(context.Background(), domain.AuditRecord{
		AgentID: agent, SituationType: domain.SituationApproval, Trigger: "t",
		Action: "escalated", Status: "escalated", Suggestion: "respond: y", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestStreamEscalationDismissedAndCorrected(t *testing.T) {
	app, st := testApp(t)
	log := withStream(t, app)
	ctx := context.Background()
	if err := st.AssignAgentName(ctx, "p1", "otter"); err != nil {
		t.Fatal(err)
	}
	dismissed := seedPendingEscalation(t, st, "p1")
	answered := seedPendingEscalation(t, st, "p1")
	if err := app.Dismiss(ctx, dismissed); err != nil {
		t.Fatal(err)
	}
	if err := app.Resolve(ctx, answered, "Yes", false); err != nil {
		t.Fatal(err)
	}
	got := streamLines(t, log, 0)
	if len(got) != 2 || got[0] != "escalation.dismissed id="+itoa(dismissed)+" by=operator" ||
		!strings.HasPrefix(got[1], "correction id=") ||
		!strings.HasSuffix(got[1], " escalation="+itoa(answered)+" agent=otter send=false by=operator") {
		t.Fatalf("stream events = %v", got)
	}
}

func TestStreamRuleEdits(t *testing.T) {
	app, st := testApp(t)
	log := withStream(t, app)
	ctx := context.Background()
	seedAdjustable(t, st, "approval:nudge-stream", 2)
	if _, err := app.AdjustSignatureConfirmations(ctx, "approval:nudge-s", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := app.ResetSignatureGraduation(ctx, "approval:nudge-s"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := app.DeleteSignature(ctx, "approval:nudge-s"); err != nil {
		t.Fatal(err)
	}
	got := streamLines(t, log, 0)
	if len(got) != 3 || !strings.HasPrefix(got[0], "rule.streak sig=approval:nud delta=+1 streak=1 mode=") ||
		got[1] != "rule.reset sig=approval:nud by=operator" ||
		got[2] != "rule.deleted sig=approval:nud by=operator" {
		t.Fatalf("stream events = %v", got)
	}
}

// With no stream wired every chokepoint must still work — the field is
// optional, and most of this suite never sets it.
func TestNoStreamIsANoOp(t *testing.T) {
	app, _ := testApp(t)
	if _, err := app.Pause(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := app.SetField(context.Background(), "full_self_prompting.honour_limits", "true"); err != nil {
		t.Fatal(err)
	}
}

func itoa(n int64) string { return domain.StreamInt("", n).Value }
