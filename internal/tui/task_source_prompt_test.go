package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

// sourcePromptModel wires a real store-backed App with no task sources yet,
// so the add prompt writes through the same path the CLI uses.
func sourcePromptModel(t *testing.T) (Model, *frontend.App, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	app := &frontend.App{Store: st, Herdr: &captureHerdr{},
		ConfigPath: filepath.Join(dir, "config.toml"), Author: "operator"}
	path := filepath.Join(dir, "tasks.md")
	if err := os.WriteFile(path, []byte("- [ ] alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := New(context.Background(), app)
	m.width, m.height = 100, 30
	return m, app, path
}

// submitSourcePrompt opens the add-task-source prompt, types input, and runs
// the resulting command as Bubble Tea's runtime would.
func submitSourcePrompt(t *testing.T, m Model, input string) actionResultMsg {
	t.Helper()
	upd, _ := m.addTaskSourcePrompt()
	m = upd.(Model)
	if m.prompt == nil {
		t.Fatal("addTaskSourcePrompt did not open a prompt")
	}
	if !strings.Contains(m.prompt.label, "--auto-send-when-idle") {
		t.Errorf("prompt label must advertise the flag, got %q", m.prompt.label)
	}
	cmd := m.prompt.onSubmit(input)
	if cmd == nil {
		t.Fatal("onSubmit returned no command")
	}
	msg, ok := cmd().(actionResultMsg)
	if !ok {
		t.Fatalf("want actionResultMsg, got %T", msg)
	}
	return msg
}

// TestTUIAddTaskSourceAutoSendParity pins CLI/TUI parity for the one source
// setting that makes hap hand out tasks unprompted: the TUI prompt takes the
// same `--auto-send-when-idle` word, in any position, and — like the CLI — an
// add that does not ask for it must never turn it on.
func TestTUIAddTaskSourceAutoSendParity(t *testing.T) {
	tests := []struct {
		name  string
		input string // %s is replaced by the checklist path
		want  bool
	}{
		{name: "path only", input: "%s"},
		{name: "path and agent", input: "%s brave-otter"},
		{name: "flag last", input: "%s brave-otter --auto-send-when-idle", want: true},
		{name: "flag first", input: "--auto-send-when-idle %s brave-otter", want: true},
		{name: "flag mid", input: "%s --auto-send-when-idle brave-otter ws-1", want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, app, path := sourcePromptModel(t)
			msg := submitSourcePrompt(t, m, strings.ReplaceAll(tc.input, "%s", path))
			if msg.err != nil {
				t.Fatal(msg.err)
			}
			cfg, err := app.Config()
			if err != nil {
				t.Fatal(err)
			}
			if len(cfg.TaskSources) != 1 {
				t.Fatalf("want 1 task source, got %d", len(cfg.TaskSources))
			}
			src := cfg.TaskSources[0]
			if src.Path != path {
				t.Errorf("path = %q, want %q (the flag must not be consumed as a field)", src.Path, path)
			}
			if got := src.EnableAutoSendTaskWhenIdle; got != tc.want {
				t.Fatalf("enable_auto_send_task_when_idle = %v, want %v", got, tc.want)
			}
			if said := strings.Contains(msg.message, "auto-send when idle ON"); said != tc.want {
				t.Errorf("result message announced auto-send = %v, want %v; got %q", said, tc.want, msg.message)
			}
		})
	}
}

// TestTUIAddTaskSourcePromptRejectsBadInput keeps the field-count guard honest
// now that flag words are stripped before counting: a flag-only input has no
// path, four real fields is still too many, and — the safety-relevant case —
// any near-miss spelling of the flag must be REFUSED rather than stored as a
// field, since that would silently leave unprompted hand-out off while the
// operator believes they turned it on.
func TestTUIAddTaskSourcePromptRejectsBadInput(t *testing.T) {
	for _, input := range []string{
		"--auto-send-when-idle",
		"a b c d",
		"/tmp/tasks.md --auto-send-when-idle=true",
		"/tmp/tasks.md -auto-send-when-idle",
		"/tmp/tasks.md --auto-send-when-idl",
		"/tmp/tasks.md --agent brave-otter",
	} {
		m, app, _ := sourcePromptModel(t)
		msg := submitSourcePrompt(t, m, input)
		if msg.err == nil {
			t.Errorf("input %q must be rejected, got %q", input, msg.message)
		}
		cfg, err := app.Config()
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.TaskSources) != 0 {
			t.Errorf("input %q added a source anyway: %+v", input, cfg.TaskSources)
		}
	}
}

// TestConfigTabShowsAutoSendFlag mirrors `hap task-source list`: a source that
// hands out tasks unprompted must say so wherever sources are listed.
func TestConfigTabShowsAutoSendFlag(t *testing.T) {
	m, app, path := sourcePromptModel(t)
	if msg := submitSourcePrompt(t, m, path+" busy-otter --auto-send-when-idle"); msg.err != nil {
		t.Fatal(msg.err)
	}
	quiet := filepath.Join(filepath.Dir(path), "quiet.md")
	if err := os.WriteFile(quiet, []byte("- [ ] a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := app.AddTaskSource(context.Background(), "quiet-fox", "", quiet, ""); err != nil {
		t.Fatal(err)
	}

	cfg, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	var busy, calm string
	for _, it := range buildRuleItems(cfg) {
		if it.kind != "source" {
			continue
		}
		if strings.Contains(it.label, "busy-otter") {
			busy = it.label
		}
		if strings.Contains(it.label, "quiet-fox") {
			calm = it.label
		}
	}
	if busy == "" || calm == "" {
		t.Fatalf("both sources should render as config rows, got busy=%q quiet=%q", busy, calm)
	}
	if !strings.Contains(busy, "auto_send_when_idle=true") {
		t.Errorf("auto-send source row must show the flag: %s", busy)
	}
	if strings.Contains(calm, "auto_send_when_idle") {
		t.Errorf("a source without the flag must not advertise it: %s", calm)
	}
}

// editSourceSetting drives the two-step Config-tab edit of task source #index:
// the settings picker, then the value prompt the picked setting opens. It
// returns the final action result, exactly as the Bubble Tea runtime would
// produce it.
func editSourceSetting(t *testing.T, m Model, index int, path, pick, value string) actionResultMsg {
	t.Helper()
	upd, _ := m.editTaskSourcePrompt(index, path)
	m = upd.(Model)
	if m.prompt == nil || len(m.prompt.options) != 2 {
		t.Fatalf("expected a two-option settings picker, got %+v", m.prompt)
	}
	var chosen string
	for _, o := range m.prompt.options {
		if strings.HasPrefix(o, pick) {
			chosen = o
		}
	}
	if chosen == "" {
		t.Fatalf("picker has no %q option: %v", pick, m.prompt.options)
	}
	cmd := m.prompt.onSubmit(chosen)
	if cmd == nil {
		t.Fatal("settings picker returned no command")
	}
	fieldMsg, ok := cmd().(openTaskSourceFieldMsg)
	if !ok {
		t.Fatalf("picker should chain to a value prompt, got %T", cmd())
	}
	upd, _ = m.Update(fieldMsg)
	m = upd.(Model)
	if m.prompt == nil {
		t.Fatal("value prompt did not open")
	}
	cmd = m.prompt.onSubmit(value)
	if cmd == nil {
		t.Fatal("value prompt returned no command")
	}
	msg, ok := cmd().(actionResultMsg)
	if !ok {
		t.Fatalf("want actionResultMsg, got %T", cmd())
	}
	return msg
}

// TestConfigTabEditsTaskSourceSettings pins the Config tab's enter on a
// task-source row: both mutable settings are editable there, and the values
// reach config.toml (the daemon reads the file, not the model).
func TestConfigTabEditsTaskSourceSettings(t *testing.T) {
	m, app, path := sourcePromptModel(t)
	if msg := submitSourcePrompt(t, m, path+" busy-otter"); msg.err != nil {
		t.Fatal(msg.err)
	}
	cfg, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	m.data.cfg = cfg
	m.items = buildRuleItems(cfg)

	// A source starts with unprompted hand-out off and the default cap named
	// explicitly — never a bare 0, which reads as "no limit".
	if cfg.TaskSources[0].EnableAutoSendTaskWhenIdle || cfg.TaskSources[0].MaxTasks != config.DefaultMaxTasks {
		t.Fatalf("unexpected starting state: %+v", cfg.TaskSources[0])
	}

	if msg := editSourceSetting(t, m, 0, path, "auto_send_when_idle", "true"); msg.err != nil {
		t.Fatal(msg.err)
	}
	if cfg, err = app.Config(); err != nil {
		t.Fatal(err)
	}
	if !cfg.TaskSources[0].EnableAutoSendTaskWhenIdle {
		t.Error("picking true must turn unprompted hand-out on")
	}
	m.data.cfg = cfg

	if msg := editSourceSetting(t, m, 0, path, "max_tasks", "9"); msg.err != nil {
		t.Fatal(msg.err)
	}
	saved, err := config.Load(app.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if saved.TaskSources[0].MaxTasks != 9 || !saved.TaskSources[0].EnableAutoSendTaskWhenIdle {
		t.Errorf("settings did not reach config.toml: %+v", saved.TaskSources[0])
	}
	m.data.cfg = saved

	// Turning it back off writes false rather than leaving the old value.
	if msg := editSourceSetting(t, m, 0, path, "auto_send_when_idle", "false"); msg.err != nil {
		t.Fatal(msg.err)
	}
	if saved, err = config.Load(app.ConfigPath); err != nil {
		t.Fatal(err)
	}
	if saved.TaskSources[0].EnableAutoSendTaskWhenIdle {
		t.Error("picking false must turn unprompted hand-out off")
	}

	// Bad values are refused, and a stale path guard refuses the write outright.
	if msg := editSourceSetting(t, m, 0, path, "max_tasks", "none"); msg.err == nil {
		t.Error("a non-numeric max_tasks must be refused")
	}
	if msg := editSourceSetting(t, m, 0, path, "max_tasks", "0"); msg.err == nil {
		t.Error("max_tasks 0 must be refused — on disk it means unset")
	}
	// A row whose path no longer matches is refused BEFORE the picker opens —
	// prompting with another source's values is how the wrong entry gets edited.
	upd, _ := m.editTaskSourcePrompt(0, "/gone.md")
	stale := upd.(Model)
	if stale.prompt != nil || !strings.Contains(stale.message, "no longer listed") {
		t.Errorf("a stale row must refuse before prompting, got prompt=%v message=%q", stale.prompt != nil, stale.message)
	}
	if saved, err = config.Load(app.ConfigPath); err != nil {
		t.Fatal(err)
	}
	if saved.TaskSources[0].MaxTasks != 9 {
		t.Errorf("a refused edit must leave the value alone, got %d", saved.TaskSources[0].MaxTasks)
	}
}

// TestConfigTabSourceRowShowsMaxTasks: the row names the cap enter is about to
// edit, resolved through MaxTasksLimit so a config written before the cap was
// filled in still shows the number the daemon enforces.
func TestConfigTabSourceRowShowsMaxTasks(t *testing.T) {
	cfg := config.Default()
	cfg.TaskSources = []config.TaskSource{
		{Agent: "a1", Path: "/tmp/one.md"},              // unset → default
		{Agent: "a2", Path: "/tmp/two.md", MaxTasks: 3}, // explicit
	}
	var rows []string
	for _, it := range buildRuleItems(cfg) {
		if it.kind == "source" {
			rows = append(rows, it.label)
		}
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 source rows, got %d", len(rows))
	}
	if !strings.Contains(rows[0], fmt.Sprintf("max_tasks=%d", config.DefaultMaxTasks)) {
		t.Errorf("an unset cap must render as the effective default: %s", rows[0])
	}
	if !strings.Contains(rows[1], "max_tasks=3") {
		t.Errorf("an explicit cap must render verbatim: %s", rows[1])
	}
}
