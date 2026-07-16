package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

// sendRecorderHerdr records deliveries and serves a fixed live-agent list.
type sendRecorderHerdr struct {
	agents []domain.AgentTransition
	sent   []string
}

func (f *sendRecorderHerdr) Send(_ context.Context, _, input string) error {
	f.sent = append(f.sent, input)
	return nil
}
func (f *sendRecorderHerdr) ReadPane(context.Context, string, int) (string, error) { return "", nil }
func (f *sendRecorderHerdr) ListAgents(context.Context) ([]domain.AgentTransition, error) {
	return f.agents, nil
}

// sendTestApp wires a real store-backed App to one task source for agent
// "w1:p1" (pending alpha with a stored `\n`, done beta) and an idle live
// agent behind a recording herdr fake.
func sendTestApp(t *testing.T, status string) (*frontend.App, *sendRecorderHerdr, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h := &sendRecorderHerdr{agents: []domain.AgentTransition{
		{AgentID: "w1:p1", PaneID: "w1:p1", AgentType: "claude", Status: status},
	}}
	app := &frontend.App{Store: st, Herdr: h,
		ConfigPath: filepath.Join(dir, "config.toml"), Author: "operator"}
	path := filepath.Join(dir, "tasks.md")
	if err := os.WriteFile(path, []byte(`- [ ] alpha\nwith detail`+"\n- [x] beta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := app.AddTaskSource(context.Background(), "w1:p1", "", path, ""); err != nil {
		t.Fatal(err)
	}
	return app, h, path
}

func runSend(t *testing.T, app *frontend.App, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := Run(context.Background(), app, &out, "task", args)
	return out.String(), err
}

func TestTaskSendYesFlagDeliversAndMarksInProgress(t *testing.T) {
	app, h, path := sendTestApp(t, "idle")
	out, err := runSend(t, app, "w1:p1", "send", "1", "--yes")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "task #1 sent to w1:p1 and marked [-] in progress") {
		t.Errorf("missing success message, got %q", out)
	}
	if len(h.sent) != 1 || !strings.Contains(h.sent[0], "alpha\nwith detail") {
		t.Errorf("prompt should carry the decoded task text, got %v", h.sent)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `- [-] alpha\nwith detail`) {
		t.Errorf("sent task should be marked [-], got %q", data)
	}
}

func TestTaskSendConfirmation(t *testing.T) {
	app, h, path := sendTestApp(t, "idle")
	// Default answer (empty/N) aborts and leaves everything untouched.
	old := stdin
	t.Cleanup(func() { stdin = old })
	stdin = strings.NewReader("n\n")
	out, err := runSend(t, app, "w1:p1", "send", "1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[y/N]") || !strings.Contains(out, "aborted — task unchanged") {
		t.Errorf("n should abort with the prompt shown, got %q", out)
	}
	if len(h.sent) != 0 {
		t.Errorf("aborted send must not deliver, got %v", h.sent)
	}
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), `- [ ] alpha`) {
		t.Errorf("aborted send must keep the task pending, got %q", data)
	}
	// y proceeds.
	stdin = strings.NewReader("y\n")
	if out, err = runSend(t, app, "w1:p1", "send", "1"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "sent to w1:p1") || len(h.sent) != 1 {
		t.Errorf("y should deliver, got out=%q sent=%v", out, h.sent)
	}
	// A scripted (non-TTY os.Stdin) run without --yes refuses instead of
	// silently no-oping with exit 0.
	stdin = os.Stdin
	app2, h2, _ := sendTestApp(t, "idle")
	if _, err := runSend(t, app2, "w1:p1", "send", "1"); err == nil ||
		!strings.Contains(err.Error(), "--yes") {
		t.Errorf("non-TTY confirmation must refuse with a --yes hint, got %v", err)
	}
	if len(h2.sent) != 0 {
		t.Errorf("refused non-TTY send must not deliver, got %v", h2.sent)
	}
}

func TestOneLineTextRuneSafe(t *testing.T) {
	got := oneLineText(strings.Repeat("계획", 40), 10)
	if r := []rune(got); len(r) != 10 || !strings.HasSuffix(got, "…") {
		t.Errorf("truncation must count runes, got %q (%d runes)", got, len(r))
	}
	if got := oneLineText("a\nb", 10); got != "a b" {
		t.Errorf("newlines should flatten, got %q", got)
	}
}

func TestTaskSendRefusals(t *testing.T) {
	// Done task.
	app, h, _ := sendTestApp(t, "idle")
	if _, err := runSend(t, app, "w1:p1", "send", "2", "--yes"); err == nil ||
		!strings.Contains(err.Error(), "only a pending [ ] task") {
		t.Errorf("done task must refuse, got %v", err)
	}
	if len(h.sent) != 0 {
		t.Errorf("refused send must not deliver, got %v", h.sent)
	}
	// Busy agent.
	app, h, _ = sendTestApp(t, "working")
	if _, err := runSend(t, app, "w1:p1", "send", "1", "--yes"); err == nil ||
		!strings.Contains(err.Error(), "cleanly idle") {
		t.Errorf("busy agent must refuse, got %v", err)
	}
	if len(h.sent) != 0 {
		t.Errorf("refused send must not deliver, got %v", h.sent)
	}
	// Unknown agent name.
	app, h, _ = sendTestApp(t, "idle")
	if _, err := runSend(t, app, "nobody", "send", "1", "--yes"); err == nil {
		t.Error("unknown agent must refuse")
	}
	if len(h.sent) != 0 {
		t.Errorf("refused sends must not deliver, got %v", h.sent)
	}
}
