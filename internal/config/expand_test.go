package config

import (
	"path/filepath"
	"testing"
)

func TestExpandPath(t *testing.T) {
	// A stable, known home for the tilde cases. os.UserHomeDir consults $HOME
	// first on Unix, so setting it makes the expansion deterministic.
	home := "/home/tester"
	t.Setenv("HOME", home)
	t.Setenv("HAP_TEST_DIR", "/srv/data")
	t.Setenv("HAP_TEST_EMPTY", "")

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"tilde alone", "~", home},
		{"tilde slash", "~/tasks.md", filepath.Join(home, "tasks.md")},
		{"tilde nested", "~/a/b/tasks.md", filepath.Join(home, "a/b/tasks.md")},
		{"env var bare", "$HAP_TEST_DIR/tasks.md", "/srv/data/tasks.md"},
		{"env var braced", "${HAP_TEST_DIR}/tasks.md", "/srv/data/tasks.md"},
		{"env then tilde", "$HOME/tasks.md", filepath.Join(home, "tasks.md")},
		{"undefined var drops to empty", "$HAP_TEST_MISSING/tasks.md", "/tasks.md"},
		{"empty var", "${HAP_TEST_EMPTY}/tasks.md", "/tasks.md"},
		{"absolute unchanged", "/etc/hap/tasks.md", "/etc/hap/tasks.md"},
		{"relative unchanged", "tasks/list.md", "tasks/list.md"},
		{"relative dot unchanged", "./tasks.md", "./tasks.md"},
		{"tilde not a prefix", "~user/tasks.md", "~user/tasks.md"},
		{"tilde mid-path is literal", "/opt/~/tasks.md", "/opt/~/tasks.md"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExpandPath(tc.in); got != tc.want {
				t.Errorf("ExpandPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestExpandPathIdempotent confirms a second pass is a no-op: once ~/$VAR are
// resolved to an absolute path, re-expanding must not change it. Callers that
// expand at more than one layer (e.g. canonicalTaskPath then the daemon's
// ReadTaskFile wrapper) rely on this.
func TestExpandPathIdempotent(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	for _, in := range []string{"~/tasks.md", "$HOME/tasks.md", "/etc/tasks.md", "rel.md"} {
		once := ExpandPath(in)
		if twice := ExpandPath(once); twice != once {
			t.Errorf("ExpandPath not idempotent for %q: %q then %q", in, once, twice)
		}
	}
}
