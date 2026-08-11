package cli_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// `hap config task-source` is SelfHints, so the registry no longer supplies its
// footer — every branch has to print its own. That created a failure mode no
// existing test could see: TestNoCommandPrintsTwoFooters asserts `n > 1`, so a
// branch printing ZERO footers passes it, and TestRegistryEntriesAreDocumented's
// "no next-step hints" check is satisfied by the SelfHints flag alone.
//
// These cases assert exactly one footer, naming a follow-up that makes sense
// from where the operator is standing.
func TestTaskSourceEveryBranchPrintsItsOwnFooter(t *testing.T) {
	countFooters := func(s string) int { return strings.Count(s, "\nNext steps:\n") }

	t.Run("empty listing points at add, not at itself", func(t *testing.T) {
		app, _ := testApp(t)
		out, err := run(t, app, "config", "task-source", "list")
		if err != nil {
			t.Fatal(err)
		}
		if n := countFooters(out); n != 1 {
			t.Fatalf("printed %d footers, want exactly 1:\n%s", n, out)
		}
		// Telling an operator with no sources to run the listing they just ran
		// leaves them exactly where they started.
		if !strings.Contains(out, "hap config task-source add") {
			t.Errorf("the empty listing must point at `add`, got:\n%s", out)
		}
		if strings.Contains(out, "- `hap config task-source list`") {
			t.Errorf("the empty listing suggests itself:\n%s", out)
		}
	})

	app, _ := testApp(t)
	ctx := context.Background()
	dir := t.TempDir()
	if err := app.AddTaskSource(ctx, "brave-otter", "", filepath.Join(dir, "a.md"), ""); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		// The listing names a REAL source, addressed by the agent it feeds.
		{"list", []string{"list"}, "hap config task-source set brave-otter"},
		{"set", []string{"set", "brave-otter", "max-tasks", "9"}, "hap config task-source list"},
		{"provider", []string{"provider"}, "hap config task-source list"},
		{"add", []string{"add", "--agent", "swift-heron", filepath.Join(dir, "b.md")}, "hap config task-source list"},
		// Removal renumbers every later source, so its footer must lead back to
		// the listing before another reference is used.
		{"remove", []string{"remove", "brave-otter"}, "hap config task-source list"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := run(t, app, "config", append([]string{"task-source"}, tc.args...)...)
			if err != nil {
				t.Fatalf("%v: %v", tc.args, err)
			}
			if n := countFooters(out); n != 1 {
				t.Fatalf("printed %d footers, want exactly 1:\n%s", n, out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("footer does not suggest %q:\n%s", tc.want, out)
			}
		})
	}
}

// TestTaskSourceListFooterNeverSuggestsARefusedCommand pins the bound on
// addressing the footer by name: two sources feeding one agent is legal, and
// naming that agent would emit a command `resolveTaskSourceRef` refuses — on
// the very screen that exists to resolve the ambiguity.
func TestTaskSourceListFooterNeverSuggestsARefusedCommand(t *testing.T) {
	app, _ := testApp(t)
	ctx := context.Background()
	dir := t.TempDir()
	for _, f := range []string{"a.md", "b.md"} {
		if err := app.AddTaskSource(ctx, "brave-otter", "", filepath.Join(dir, f), ""); err != nil {
			t.Fatal(err)
		}
	}
	out, err := run(t, app, "config", "task-source", "list")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "set brave-otter") || strings.Contains(out, "remove brave-otter") {
		t.Errorf("the footer suggests an ambiguous reference that would be refused:\n%s", out)
	}
	if !strings.Contains(out, "hap config task-source set 0") {
		t.Errorf("the footer must fall back to the index when the name is ambiguous:\n%s", out)
	}
}

// TestNumericAgentSelectorIsRefused keeps the index-vs-name discrimination true
// BY CONSTRUCTION rather than by assertion. Stored, `--agent 3` would make that
// source permanently unaddressable by name while `set 3 …` silently meant index
// 3 — so it is refused where it would be written, not worked around at the
// point of use.
func TestNumericAgentSelectorIsRefused(t *testing.T) {
	app, _ := testApp(t)
	ctx := context.Background()
	dir := t.TempDir()

	if err := app.AddTaskSource(ctx, "3", "", filepath.Join(dir, "a.md"), ""); err == nil {
		t.Error("`add --agent 3` must be refused: the CLI reads a bare number as an index")
	}
	if err := app.AddTaskSource(ctx, "brave-otter", "", filepath.Join(dir, "a.md"), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, app, "config", "task-source", "set", "0", "agent", "7"); err == nil {
		t.Error("`set 0 agent 7` must be refused for the same reason")
	}
	// A real pane/agent id is NOT numeric and must still be accepted.
	if _, err := run(t, app, "config", "task-source", "set", "0", "agent", "1-1"); err != nil {
		t.Errorf("a pane-id selector must still be accepted: %v", err)
	}
	if got := loadCfg(t, app.ConfigPath).TaskSources[0].Agent; got != "1-1" {
		t.Errorf("Agent = %q, want the pane id stored", got)
	}
}
