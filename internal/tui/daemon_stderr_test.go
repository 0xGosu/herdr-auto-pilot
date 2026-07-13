package tui

import (
	"os"
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/daemonhealth"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
)

// `!` opens the captured daemon stderr as a detail when the daemon is in an
// error state, surfacing the crash reason in-app (#83).
func TestDaemonStderrKeyOpensDetailOnError(t *testing.T) {
	dir := t.TempDir()
	logPath := daemonhealth.StderrLogPath(dir)
	if err := os.WriteFile(logPath, []byte("GGML_ASSERT(i01 < ne01) failed\nggml_abort\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := Model{width: 100, height: 30, app: &frontend.App{StateDir: dir}}
	m.data.daemonHealth = frontend.DaemonHealth{
		CrashLooping: true, RecentRestarts: 3, StderrLog: logPath,
	}

	upd, _ := m.Update(pressKeyMsg("!"))
	got := upd.(Model)
	if got.detail == nil {
		t.Fatal("`!` in an error state must open the stderr detail")
	}
	if body := strings.Join(got.detail.lines, "\n"); !strings.Contains(body, "GGML_ASSERT") {
		t.Errorf("detail missing the captured crash reason:\n%s", body)
	}
}

// `!` is a guarded no-op when the daemon is healthy — there is no crash to show.
func TestDaemonStderrKeyNoopWhenHealthy(t *testing.T) {
	m := Model{width: 100, height: 30, app: &frontend.App{StateDir: t.TempDir()}}
	m.data.daemonHealth = frontend.DaemonHealth{Running: true} // DaemonOK

	upd, _ := m.Update(pressKeyMsg("!"))
	got := upd.(Model)
	if got.detail != nil {
		t.Error("`!` must not open a detail when the daemon is healthy")
	}
	if !strings.Contains(got.message, "no captured output") {
		t.Errorf("expected the guard message, got %q", got.message)
	}
}

// An error state with no captured stderr shows the empty-tail notice, not a
// blank overlay.
func TestDaemonStderrKeyEmptyTail(t *testing.T) {
	m := Model{width: 100, height: 30, app: &frontend.App{StateDir: t.TempDir()}}
	m.data.daemonHealth = frontend.DaemonHealth{GaveUp: true, Reason: "boom"}

	upd, _ := m.Update(pressKeyMsg("!"))
	got := upd.(Model)
	if got.detail == nil {
		t.Fatal("`!` in an error state must open a detail even with no captured output")
	}
	if body := strings.Join(got.detail.lines, "\n"); !strings.Contains(body, "no captured stderr") {
		t.Errorf("empty tail should show the notice, got:\n%s", body)
	}
}

// The banner hint appears only for an error-severity daemon that has a captured
// stderr log — never for a degraded (warn) daemon.
func TestDaemonStderrHintOnlyOnError(t *testing.T) {
	logPath := daemonhealth.StderrLogPath(t.TempDir())

	e := Model{width: 100, height: 30}
	e.data.daemonHealth = frontend.DaemonHealth{CrashLooping: true, RecentRestarts: 2, StderrLog: logPath}
	if !strings.Contains(e.View(), "press ! for captured output") {
		t.Errorf("error banner with a stderr log must show the hint, got:\n%s", e.View())
	}

	d := Model{width: 100, height: 30}
	d.data.daemonHealth = frontend.DaemonHealth{Running: true, EmbedderDegraded: true, StderrLog: logPath}
	if strings.Contains(d.View(), "press !") {
		t.Errorf("a warn-severity banner must not show the stderr hint, got:\n%s", d.View())
	}
}
