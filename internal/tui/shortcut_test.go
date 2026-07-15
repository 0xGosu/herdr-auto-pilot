package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
)

func shortcutModel(t *testing.T, install func() error) Model {
	t.Helper()
	m := configModel(t, config.Default())
	m.cursor = itemIndex(t, m, func(item ruleItem) bool {
		return item.kind == "shortcut" && item.key == "install-hap"
	})
	m.installShortcut = install
	return m
}

func TestConfigQuickShortcutsSectionIsLast(t *testing.T) {
	m := shortcutModel(t, func() error { return nil })
	view := m.View()

	header := strings.LastIndex(view, "Quick Shortcuts")
	row := strings.LastIndex(view, "Create /usr/local/bin/hap symlink to this running binary")
	if header < 0 || row < header {
		t.Fatalf("Quick Shortcuts section or install row missing:\n%s", view)
	}
	for _, earlier := range []string{"Config\n", "Scoped never-auto rules", "Capture delays", "Never-auto patterns", "Task sources"} {
		if pos := strings.LastIndex(view, earlier); pos > header {
			t.Errorf("%q rendered below Quick Shortcuts:\n%s", earlier, view)
		}
	}
}

func TestConfigShortcutRequiresConfirmation(t *testing.T) {
	runs := 0
	m := shortcutModel(t, func() error {
		runs++
		return nil
	})

	m = press(t, m, "enter")
	if m.confirm == nil || !strings.Contains(m.confirm.label, "[Y/n]") {
		t.Fatalf("enter on shortcut should open Y/n confirmation, got %+v", m.confirm)
	}
	if view := m.View(); !strings.Contains(view, "currently running hap binary? [Y/n]") ||
		!strings.Contains(view, "y/enter: confirm  n/esc: cancel") {
		t.Fatalf("confirmation and its keys should be visible:\n%s", view)
	}
	if runs != 0 {
		t.Fatal("shortcut ran before confirmation")
	}

	m = press(t, m, "n")
	if m.confirm != nil || m.message != "cancelled" || runs != 0 {
		t.Fatalf("n should cancel without running: confirm=%v message=%q runs=%d", m.confirm != nil, m.message, runs)
	}
}

func TestConfigShortcutYesAndDefaultEnterExecute(t *testing.T) {
	for _, key := range []string{"y", "enter"} {
		t.Run(key, func(t *testing.T) {
			runs := 0
			m := shortcutModel(t, func() error {
				runs++
				return nil
			})
			m = press(t, m, "enter")

			updated, cmd := m.Update(pressKeyMsg(key))
			m = updated.(Model)
			if cmd == nil {
				t.Fatal("confirmation should return the install command")
			}
			if runs != 0 {
				t.Fatal("install command must remain asynchronous until Bubble Tea runs it")
			}
			msg, ok := cmd().(actionResultMsg)
			if !ok || msg.err != nil || msg.message != "created /usr/local/bin/hap symlink" {
				t.Fatalf("unexpected shortcut result: %+v", msg)
			}
			if runs != 1 || m.confirm != nil {
				t.Fatalf("shortcut runs=%d confirm=%v, want one run and closed confirmation", runs, m.confirm != nil)
			}
		})
	}
}

func TestEnsureExecutableSymlinkCreatesAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "hap-current")
	target := filepath.Join(dir, "bin", "hap")
	if err := os.WriteFile(source, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := ensureExecutableSymlink(source, target); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	linked, err := os.Readlink(target)
	if err != nil {
		t.Fatal(err)
	}
	// ensureExecutableSymlink resolves the source through EvalSymlinks before
	// linking, so the expected value must resolve it the same way. On macOS a
	// t.TempDir() path lives under the /var → /private/var symlink, so a plain
	// filepath.Abs(source) would mismatch the resolved link target (no-op on
	// Linux, where the temp dir has no such indirection).
	resolved, err := filepath.EvalSymlinks(source)
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.Abs(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if linked != want {
		t.Fatalf("symlink source = %q, want currently running binary %q", linked, want)
	}
	if err := ensureExecutableSymlink(source, target); err != nil {
		t.Fatalf("same symlink should be idempotent: %v", err)
	}
}

func TestEnsureExecutableSymlinkRefusesExistingPath(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "hap-current")
	target := filepath.Join(dir, "hap")
	if err := os.WriteFile(source, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("keep me"), 0o755); err != nil {
		t.Fatal(err)
	}

	err := ensureExecutableSymlink(source, target)
	if err == nil || !strings.Contains(err.Error(), "not a symlink") {
		t.Fatalf("expected refusal for existing file, got %v", err)
	}
	got, readErr := os.ReadFile(target)
	if readErr != nil || string(got) != "keep me" {
		t.Fatalf("existing target was changed: content=%q err=%v", got, readErr)
	}
}

func TestEnsureExecutableSymlinkRefusesDifferentLink(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "hap-current")
	other := filepath.Join(dir, "hap-other")
	target := filepath.Join(dir, "hap")
	for _, path := range []string{source, other} {
		if err := os.WriteFile(path, []byte("binary"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(other, target); err != nil {
		t.Fatal(err)
	}

	err := ensureExecutableSymlink(source, target)
	if err == nil || !strings.Contains(err.Error(), "different target") {
		t.Fatalf("expected refusal for unrelated symlink, got %v", err)
	}
	linked, readErr := os.Readlink(target)
	if readErr != nil || linked != other {
		t.Fatalf("existing symlink was changed: target=%q err=%v", linked, readErr)
	}
}
