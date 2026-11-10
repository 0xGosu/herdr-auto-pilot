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
	t.Setenv("HOME", "/nonexistent-home") // paths below are shown unabbreviated
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

// A stale link the operator cannot write is stepped over (internal/shortcut),
// so the row acts somewhere else than they expect: the label says why before
// they press, and the result leads with the one command that needs root —
// both on a line the pane truncates.
func TestShortcutRowExplainsABlockedLink(t *testing.T) {
	t.Setenv("HOME", "/Users/op")
	link := shortcut.Link{
		Path: "/Users/op/.local/bin/hap", State: shortcut.Absent, OnPath: true,
		Blocked: "/usr/local/bin/hap", Shadows: true,
	}
	label := shortcutLabel(link)
	if !strings.HasPrefix(label, "Create ~/.local/bin/hap symlink") ||
		!strings.Contains(label, "/usr/local/bin/hap is stale and needs sudo") {
		t.Errorf("label = %q, want the new location and why the old link is not used", label)
	}
	result := shortcutResult(link)
	if want := "created ~/.local/bin/hap symlink — run: sudo rm /usr/local/bin/hap"; !strings.HasPrefix(result, want) {
		t.Errorf("result = %q, want it to start %q", result, want)
	}

	// A blocked link that does not win on PATH is litter: the label still
	// explains the location, but the result sends nobody after it with sudo.
	harmless := link
	harmless.Shadows = false
	if got := shortcutResult(harmless); got != "created ~/.local/bin/hap symlink" {
		t.Errorf("result for a harmless blocked link = %q, want no follow-up", got)
	}
	if got := shortcutLabel(harmless); !strings.Contains(got, "/usr/local/bin/hap is stale and needs sudo") {
		t.Errorf("label for a harmless blocked link = %q, want it to still say why", got)
	}

	// Both follow-ups at once: the root step first, it is the one that
	// cannot be guessed.
	link.OnPath = false
	result = shortcutResult(link)
	rm, path := strings.Index(result, "sudo rm"), strings.Index(result, "add ~/.local/bin to your PATH")
	if rm < 0 || path < 0 || rm > path {
		t.Errorf("result = %q, want the sudo step, then the PATH step", result)
	}
}

func TestTildePath(t *testing.T) {
	t.Setenv("HOME", "/Users/op")
	for in, want := range map[string]string{
		"/Users/op/.local/bin/hap": "~/.local/bin/hap",
		"/Users/op":                "~",
		"/Users/operator/bin/hap":  "/Users/operator/bin/hap", // a prefix, not the home
		"/usr/local/bin/hap":       "/usr/local/bin/hap",
	} {
		if got := tildePath(in); got != want {
			t.Errorf("tildePath(%q) = %q, want %q", in, got, want)
		}
	}
}

// A link created outside PATH succeeds and still leaves `hap` unfound; the
// result has to say so, or the operator sees "created" and a shell that
// disagrees.
func TestShortcutResultWarnsWhenTheDirectoryIsNotOnPath(t *testing.T) {
	t.Setenv("HOME", "/nonexistent-home")
	link := shortcut.Link{Path: "/home/op/.local/bin/hap", State: shortcut.Absent}
	if got := shortcutResult(link); !strings.Contains(got, "add /home/op/.local/bin to your PATH") {
		t.Errorf("result for an off-PATH link = %q, want the PATH instruction", got)
	}
	link.OnPath = true
	if got := shortcutResult(link); strings.Contains(got, "PATH") {
		t.Errorf("result for an on-PATH link = %q, want no PATH note", got)
	}
}
