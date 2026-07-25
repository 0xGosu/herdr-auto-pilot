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
	m.cursors[m.tab] = itemIndex(t, m, func(item ruleItem) bool {
		return item.kind == "shortcut" && item.key == "install-hap"
	})
	m.installShortcut = install
	return m
}

func TestConfigQuickShortcutsSectionIsLast(t *testing.T) {
	m := shortcutModel(t, func() error { return nil })
	view := m.View()

	header := strings.LastIndex(view, "Quick Shortcuts")
	// The row's verb depends on what /usr/local/bin/hap currently holds
	// (create / repoint / recreate), so match the part that never changes.
	row := strings.LastIndex(view, "/usr/local/bin/hap symlink")
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
			if !ok || msg.err != nil || !strings.Contains(msg.message, "/usr/local/bin/hap symlink") {
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

// A plugin upgrade installs the new release in a new directory and removes the
// old one, leaving this shortcut dangling. Refusing to touch it (the original
// behaviour) left the operator's `hap` command permanently broken with no
// in-app remedy, so a dead link is now repointed.
func TestEnsureExecutableSymlinkRepointsDanglingLink(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "new-install", "bin", "hap")
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "hap")
	if err := os.Symlink(filepath.Join(dir, "removed-install", "bin", "hap"), target); err != nil {
		t.Fatal(err)
	}

	if err := ensureExecutableSymlink(source, target); err != nil {
		t.Fatalf("dangling link should be repointed, got %v", err)
	}
	linked, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatalf("repointed link does not resolve: %v", err)
	}
	want, err := filepath.EvalSymlinks(source)
	if err != nil {
		t.Fatal(err)
	}
	if linked != want {
		t.Fatalf("symlink resolves to %q, want the running binary %q", linked, want)
	}
}

// An upgrade can also leave the link pointing at a still-present OLD plugin
// install. That is a hap we put there, so it is superseded — unlike an
// unrelated binary, which the refusal test above protects.
func TestEnsureExecutableSymlinkRepointsPreviousPluginInstall(t *testing.T) {
	dir := t.TempDir()
	// Ownership is anchored at THIS machine's install root, so the test has to
	// place its fake installs inside it.
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	source := filepath.Join(dir, "config", "herdr", "plugins", "github", "herd-auto-prompter-0.4.51", "bin", "hap")
	old := filepath.Join(dir, "config", "herdr", "plugins", "github", "herd-auto-prompter-0.4.50", "bin", "hap")
	for _, path := range []string{source, old} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("binary"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(dir, "hap")
	if err := os.Symlink(old, target); err != nil {
		t.Fatal(err)
	}

	if err := ensureExecutableSymlink(source, target); err != nil {
		t.Fatalf("link to a previous plugin install should be repointed, got %v", err)
	}
	linked, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(source)
	if err != nil {
		t.Fatal(err)
	}
	if linked != want {
		t.Fatalf("symlink resolves to %q, want %q", linked, want)
	}
}

// The repoint must never leave the path empty: a shell resolving `hap` while
// it happens has to see either the old link or the new one.
func TestRepointSymlinkIsAtomic(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "hap-new")
	if err := os.WriteFile(source, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "hap")
	if err := os.Symlink(filepath.Join(dir, "gone"), target); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- repointSymlink(source, target) }()
	// Poll Lstat throughout: rename(2) replaces the entry in one step, so the
	// name must resolve at every observation.
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("repointSymlink: %v", err)
			}
			if _, err := os.Lstat(target); err != nil {
				t.Fatalf("target missing after repoint: %v", err)
			}
			return
		default:
			if _, err := os.Lstat(target); err != nil {
				t.Fatalf("target disappeared mid-repoint: %v", err)
			}
		}
	}
}

func TestClassifyShortcut(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "hap-current")
	other := filepath.Join(dir, "hap-other")
	for _, path := range []string{source, other} {
		if err := os.WriteFile(path, []byte("binary"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := func(t *testing.T, name, dest string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.Symlink(dest, path); err != nil {
			t.Fatal(err)
		}
		return path
	}
	plainFile := filepath.Join(dir, "not-a-link")
	if err := os.WriteFile(plainFile, []byte("keep me"), 0o755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		target  string
		want    shortcutState
		wantErr bool
	}{
		{"absent", filepath.Join(dir, "nothing-here"), shortcutAbsent, false},
		{"already ours", link(t, "current-link", source), shortcutCurrent, false},
		{"dangling", link(t, "dead-link", filepath.Join(dir, "removed")), shortcutStale, false},
		{"live foreign binary", link(t, "foreign-link", other), shortcutForeign, true},
		{"regular file", plainFile, shortcutForeign, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := classifyShortcut(source, tt.target)
			if got != tt.want {
				t.Errorf("classifyShortcut = %v, want %v", got, tt.want)
			}
			if (err != nil) != tt.wantErr {
				t.Errorf("classifyShortcut err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestShortcutLabelsMatchState(t *testing.T) {
	tests := []struct {
		state      shortcutState
		wantLabel  string
		wantResult string
	}{
		{shortcutAbsent, "Create", "created"},
		{shortcutCurrent, "Recreate", "created"},
		{shortcutStale, "Repoint", "repointed"},
		{shortcutForeign, "Create", "created"},
	}
	for _, tt := range tests {
		if got := shortcutLabel(tt.state); !strings.HasPrefix(got, tt.wantLabel) {
			t.Errorf("shortcutLabel(%v) = %q, want it to start with %q", tt.state, got, tt.wantLabel)
		}
		if got := shortcutResult(tt.state); !strings.HasPrefix(got, tt.wantResult) {
			t.Errorf("shortcutResult(%v) = %q, want it to start with %q", tt.state, got, tt.wantResult)
		}
		if got := shortcutConfirm(tt.state); !strings.HasSuffix(got, "[Y/n]") {
			t.Errorf("shortcutConfirm(%v) = %q, want a Y/n prompt", tt.state, got)
		}
	}
}
