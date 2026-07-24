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
// ReadTaskFile wrapper, or MutateWithin then LockPath) rely on this — otherwise
// the lock key would diverge from the file actually read/written.
func TestExpandPathIdempotent(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	// A variable whose VALUE is itself another $VAR reference: os.ExpandEnv is
	// single-pass, so without fixed-point iteration ExpandPath("$CHAIN_A")
	// would return "$CHAIN_B" and a second pass would then change it again.
	t.Setenv("CHAIN_B", "/srv/tasks.md")
	t.Setenv("CHAIN_A", "$CHAIN_B")
	for _, in := range []string{"~/tasks.md", "$HOME/tasks.md", "/etc/tasks.md", "rel.md", "$CHAIN_A", "${CHAIN_A}"} {
		once := ExpandPath(in)
		if twice := ExpandPath(once); twice != once {
			t.Errorf("ExpandPath not idempotent for %q: %q then %q", in, once, twice)
		}
	}
}

// TestExpandPathResolvesNestedVars confirms a $VAR whose value references
// another $VAR is resolved fully in a single ExpandPath call, so the read,
// write, and lock-key derivations for one task file all agree.
func TestExpandPathResolvesNestedVars(t *testing.T) {
	t.Setenv("CHAIN_B", "/srv/tasks.md")
	t.Setenv("CHAIN_A", "$CHAIN_B")
	if got := ExpandPath("$CHAIN_A"); got != "/srv/tasks.md" {
		t.Errorf("ExpandPath($CHAIN_A) = %q, want /srv/tasks.md", got)
	}
}

// TestExpandPathChainEndingInTilde confirms the env→tilde ordering survives
// iteration: a $VAR whose resolved value carries a leading ~ still gets the ~
// expanded (each pass does env-then-tilde), so a chain ending in "~/x" lands in
// HOME rather than a literal "~".
func TestExpandPathChainEndingInTilde(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	t.Setenv("TILDE_B", "~/tasks.md")
	t.Setenv("TILDE_A", "$TILDE_B")
	if got := ExpandPath("$TILDE_A"); got != "/home/tester/tasks.md" {
		t.Errorf("ExpandPath($TILDE_A) = %q, want /home/tester/tasks.md", got)
	}
}

// TestExpandPathTerminatesOnCycle confirms a self-referential variable cycle
// does not spin: the loop is capped, so it returns (best-effort) instead of
// hanging the caller.
func TestExpandPathTerminatesOnCycle(t *testing.T) {
	t.Setenv("CYCLE_A", "$CYCLE_B")
	t.Setenv("CYCLE_B", "$CYCLE_A")
	got := ExpandPath("$CYCLE_A") // must return, value is unspecified but bounded
	if got != "$CYCLE_A" && got != "$CYCLE_B" {
		t.Errorf("cycle expansion returned unexpected %q", got)
	}
}
