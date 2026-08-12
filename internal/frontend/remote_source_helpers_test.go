package frontend_test

import (
	"context"
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
)

// TestRemoteSourceResolvesToAGistLocatorNotACwdPath is the regression the
// reviewer caught: filepath.Abs does not FAIL on "gist://…", it silently
// produces "<cwd>/gist:/<id>/<file>". A helper that applied it would have the
// caller reserve and send against a LOCAL file of that name instead.
func TestRemoteSourceResolvesToAGistLocatorNotACwdPath(t *testing.T) {
	app, _ := testApp(t)
	ctx := context.Background()
	for _, kv := range [][2]string{
		{"task_source_provider.provider", "github_gist"},
		{"task_source_provider.github_gist.gist_id", "3f2a1b9c"},
		{"task_source_provider.env_file", "/etc/hap/task.env"},
	} {
		if _, err := app.SetField(ctx, kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.AddTaskSource(ctx, "brave-otter", "", "", ""); err != nil {
		t.Fatal(err)
	}

	got, err := app.TaskSourcePathFor("brave-otter")
	if err != nil {
		t.Fatal(err)
	}
	const want = "gist://3f2a1b9c/brave-otter.md"
	if got != want {
		t.Errorf("TaskSourcePathFor = %q, want %q — a mangled locator makes `hap task send` "+
			"reserve against a local file instead of the store", got, want)
	}
	if strings.Contains(got, "gist:/") && !strings.Contains(got, "gist://") {
		t.Errorf("the scheme was flattened by a path helper: %q", got)
	}
}

// TestRemoteSourceTemplateIsFound: the template lookup compared a bare gist
// file name (turned into a cwd path) against a locator, so it never matched and
// every remote source silently fell back to the default prompt.
func TestRemoteSourceTemplateIsFound(t *testing.T) {
	app, _ := testApp(t)
	ctx := context.Background()
	for _, kv := range [][2]string{
		{"task_source_provider.provider", "github_gist"},
		{"task_source_provider.github_gist.gist_id", "3f2a1b9c"},
		{"task_source_provider.env_file", "/etc/hap/task.env"},
	} {
		if _, err := app.SetField(ctx, kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	const tpl = "DO {next_task_content} FROM {task_list_path}"
	if err := app.AddTaskSource(ctx, "brave-otter", "", "", tpl); err != nil {
		t.Fatal(err)
	}
	locator, err := app.TaskSourcePathFor("brave-otter")
	if err != nil {
		t.Fatal(err)
	}
	got, err := app.TaskSourceTemplateFor("brave-otter", locator)
	if err != nil {
		t.Fatal(err)
	}
	if got != tpl {
		t.Errorf("template = %q, want the source's own %q — an unmatched lookup silently "+
			"sends the DEFAULT prompt instead of the operator's", got, tpl)
	}
}

// TestRemoteSourceStaysCapped: the cap lookup compared a bare gist file name
// (turned into a cwd path) against a locator, so a configured gist source read
// as UNREGISTERED and therefore uncapped — worse than having no cap, because
// the operator has set one and believes it applies.
//
// Driven through the store so the whole path is exercised, not just the lookup.
func TestRemoteSourceStaysCapped(t *testing.T) {
	app, _ := testApp(t)
	ctx := context.Background()
	for _, kv := range [][2]string{
		{"task_source_provider.provider", "github_gist"},
		{"task_source_provider.github_gist.gist_id", "3f2a1b9c"},
		{"task_source_provider.env_file", "/etc/hap/task.env"},
	} {
		if _, err := app.SetField(ctx, kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.AddTaskSource(ctx, "brave-otter", "", "", "", frontend.MaxTasks(2)); err != nil {
		t.Fatal(err)
	}
	locator, err := app.TaskSourcePathFor("brave-otter")
	if err != nil {
		t.Fatal(err)
	}
	if got := app.TaskSourceLimitForTest("brave-otter", locator); got != 2 {
		t.Errorf("cap for a configured gist source = %d, want 2 — 0 means the lookup did "+
			"not recognize it and the source runs uncapped", got)
	}
}
