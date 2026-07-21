package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
