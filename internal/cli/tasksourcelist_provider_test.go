package cli_test

import (
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
)

// TestTaskSourceListIsByteIdenticalWithoutAnyProviderConfigured is the
// compatibility promise: an install that never touched the storage setting must
// see exactly the output it always saw. Every existing script, and the
// copy-pasteable "#0" convention, depends on it.
func TestTaskSourceListIsByteIdenticalWithoutAnyProviderConfigured(t *testing.T) {
	app, _ := testApp(t)
	if _, err := run(t, app, "task-source", "add", "--agent", "brave-otter", "/tmp/tasks.md"); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, app, "task-source", "list")
	if err != nil {
		t.Fatal(err)
	}
	// Compare the ROWS only: `run` keeps the next-step footer every verb
	// prints, which is not what this test is about.
	got := taskSourceRows(out)
	want := []string{
		"#0\tagent=\"brave-otter\" workspace=\"\" path=/tmp/tasks.md " +
			"enable_llm_review_before_auto_send=false max_tasks=20",
	}
	if len(got) != len(want) || got[0] != want[0] {
		t.Errorf("list output changed for a default install:\n got %q\nwant %q", got, want)
	}
	if strings.Contains(out, "provider") {
		t.Error("a default install must not grow a provider column")
	}
}

// TestTaskSourceListDistinguishesInheritedFromOverridden: an inherited value
// and an identical explicit override must not render the same, or an operator
// cannot predict what changing the default will do to a row.
func TestTaskSourceListDistinguishesInheritedFromOverridden(t *testing.T) {
	app, _ := testApp(t)
	for _, kv := range [][2]string{
		{"task_source_provider.provider", "github_gist"},
		{"task_source_provider.github_gist.gist_id", "3f2a1b9c4d5e6f708192a3b4c5d6e7f8"},
		{"task_source_provider.env_file", "/etc/hap/task.env"},
	} {
		if _, err := run(t, app, "config", "set", kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	// Inheriting + derived per agent.
	if _, err := run(t, app, "task-source", "add", "--agent", "brave-otter"); err != nil {
		t.Fatal(err)
	}
	// Explicitly the same provider — must NOT read as inherited.
	if _, err := run(t, app, "task-source", "add",
		"--agent", "calm-badger", "--provider", "github_gist", "shared.md"); err != nil {
		t.Fatal(err)
	}
	// Overridden back to local.
	if _, err := run(t, app, "task-source", "add",
		"--agent", "legacy-fox", "--provider", "local_fs", "/tmp/legacy.md"); err != nil {
		t.Fatal(err)
	}
	// Its own gist.
	if _, err := run(t, app, "task-source", "add",
		"--agent", "secret-badger", "--gist-id", "aa11bb22cc33", "secret.md"); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, app, "task-source", "list")
	if err != nil {
		t.Fatal(err)
	}
	lines := taskSourceRows(out)
	if len(lines) != 5 {
		t.Fatalf("got %d lines, want a header plus 4 rows:\n%s", len(lines), out)
	}
	if !strings.HasPrefix(lines[0], "provider default: github_gist") {
		t.Errorf("header = %q, want the default provider named", lines[0])
	}
	if strings.Contains(lines[0], "3f2a1b9c4d5e6f708192a3b4c5d6e7f8") {
		t.Errorf("the header prints the gist id in full; a secret gist's URL is a "+
			"capability and must be elided: %q", lines[0])
	}

	cases := []struct {
		row  int
		want []string
		not  []string
	}{
		{1, []string{"provider=github_gist(inherited)", "gist_file=<agent-name>.md (per matched agent)"}, []string{"gist_id="}},
		{2, []string{"provider=github_gist(override)", `gist_file="shared.md"`}, nil},
		{3, []string{"provider=local_fs(override)", "path=/tmp/legacy.md"}, []string{"gist_file="}},
		{4, []string{"provider=github_gist(inherited)", "gist_id=aa11bb22\u2026(override)", `gist_file="secret.md"`}, nil},
	}
	for _, tc := range cases {
		for _, w := range tc.want {
			if !strings.Contains(lines[tc.row], w) {
				t.Errorf("row %d = %q, want it to contain %q", tc.row, lines[tc.row], w)
			}
		}
		for _, n := range tc.not {
			if strings.Contains(lines[tc.row], n) {
				t.Errorf("row %d = %q, must NOT contain %q", tc.row, lines[tc.row], n)
			}
		}
	}
}

func TestStatusPrintsTheTaskStoreOnlyWhenConfigured(t *testing.T) {
	t.Run("silent by default", func(t *testing.T) {
		app, _ := testApp(t)
		out, err := run(t, app, "status")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "task store:") {
			t.Errorf("an unchanged default must add no noise to status:\n%s", out)
		}
	})

	t.Run("names a misconfigured store", func(t *testing.T) {
		app, _ := testApp(t)
		if _, err := run(t, app, "config", "set",
			"task_source_provider.provider", config.ProviderGitHubGist); err != nil {
			t.Fatal(err)
		}
		out, err := run(t, app, "status")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "task store: default github_gist") {
			t.Errorf("status must name the store once one is configured:\n%s", out)
		}
		if !strings.Contains(out, "MISCONFIGURED") || !strings.Contains(out, "gist_id") {
			t.Errorf("status must surface WHY the store cannot be reached:\n%s", out)
		}
	})
}

// taskSourceRows returns the listing's own lines, dropping the shared
// next-steps footer `run` captures.
func taskSourceRows(out string) []string {
	body, _, _ := strings.Cut(out, "\nNext steps:")
	var rows []string
	for _, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		if strings.TrimSpace(line) != "" {
			rows = append(rows, line)
		}
	}
	return rows
}
