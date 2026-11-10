package tui

import (
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/shortcut"
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
	// The row's verb and path depend on what this machine's PATH holds
	// (create / repoint / recreate, wherever the link is), so match the part
	// that never changes.
	row := strings.LastIndex(view, "/hap symlink")
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
			if !ok || msg.err != nil || !strings.Contains(msg.message, "/hap symlink") {
				t.Fatalf("unexpected shortcut result: %+v", msg)
			}
			if runs != 1 || m.confirm != nil {
				t.Fatalf("shortcut runs=%d confirm=%v, want one run and closed confirmation", runs, m.confirm != nil)
			}
		})
	}
}

func TestShortcutLabelsMatchState(t *testing.T) {
	const path = "/home/op/.local/bin/hap"
	tests := []struct {
		state      shortcut.State
		wantLabel  string
		wantResult string
	}{
		{shortcut.Absent, "Create", "created"},
		{shortcut.Current, "Recreate", "created"},
		{shortcut.Stale, "Repoint", "repointed"},
		{shortcut.Foreign, "Create", "created"},
	}
	for _, tt := range tests {
		link := shortcut.Link{Path: path, State: tt.state, OnPath: true}
		// Every message names the path it acts on: the link is no longer at
		// one fixed place, so a prompt without it asks for a blind yes.
		for what, got := range map[string]string{
			"label": shortcutLabel(link), "confirm": shortcutConfirm(link), "result": shortcutResult(link),
		} {
			if !strings.Contains(got, path) {
				t.Errorf("%s for state %v = %q, want it to name %s", what, tt.state, got, path)
			}
		}
		if got := shortcutLabel(link); !strings.HasPrefix(got, tt.wantLabel) {
			t.Errorf("shortcutLabel(%v) = %q, want it to start with %q", tt.state, got, tt.wantLabel)
		}
		if got := shortcutResult(link); !strings.HasPrefix(got, tt.wantResult) {
			t.Errorf("shortcutResult(%v) = %q, want it to start with %q", tt.state, got, tt.wantResult)
		}
		if got := shortcutConfirm(link); !strings.HasSuffix(got, "[Y/n]") {
			t.Errorf("shortcutConfirm(%v) = %q, want a Y/n prompt", tt.state, got)
		}
	}
}

// A link created outside PATH succeeds and still leaves `hap` unfound; the
// result has to say so, or the operator sees "created" and a shell that
// disagrees.
func TestShortcutResultWarnsWhenTheDirectoryIsNotOnPath(t *testing.T) {
	link := shortcut.Link{Path: "/home/op/.local/bin/hap", State: shortcut.Absent}
	if got := shortcutResult(link); !strings.Contains(got, "add /home/op/.local/bin to your PATH") {
		t.Errorf("result for an off-PATH link = %q, want the PATH instruction", got)
	}
	link.OnPath = true
	if got := shortcutResult(link); strings.Contains(got, "PATH") {
		t.Errorf("result for an on-PATH link = %q, want no PATH note", got)
	}
}
