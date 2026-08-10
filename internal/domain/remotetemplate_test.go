package domain_test

import (
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// TestRemotePromptDropsThePathFallback is deliberately a literal string
// assertion: dropping `--path` IS the whole point of the remote template, and
// --path always reads a LOCAL file, so under a remote provider that clause
// names something that does not exist.
func TestRemotePromptDropsThePathFallback(t *testing.T) {
	const url = "https://gist.github.com/3f2a1b9c#file-brave-otter-md"

	local := domain.DeclaredTask{
		Task: "step two", Path: "/home/me/tasks.md", AgentName: "brave-otter",
	}.Prompt()
	if !strings.Contains(local, "--path") {
		t.Error("the LOCAL default must keep its --path fallback")
	}

	remote := domain.DeclaredTask{
		Task: "step two", Path: url, AgentName: "brave-otter", Remote: true,
	}.Prompt()
	if strings.Contains(remote, "--path") {
		t.Errorf("the remote default must not offer --path:\n%s", remote)
	}
	if !strings.Contains(remote, "hap task brave-otter list") {
		t.Errorf("the remote default must still point at the agent's own list:\n%s", remote)
	}
	if !strings.Contains(remote, "not a file on this machine") {
		t.Errorf("the remote default must say the list is not local, or the agent goes "+
			"looking for the file anyway:\n%s", remote)
	}
}

// TestRemotePromptRendersTheDisplayAddress pins that {task_list_path} is
// whatever the caller passed as Path — the caller's job is to pass the DISPLAY
// address, never the locator.
func TestRemotePromptRendersTheDisplayAddress(t *testing.T) {
	const url = "https://gist.github.com/3f2a1b9c#file-brave-otter-md"
	got := domain.DeclaredTask{
		Task: "x", Path: url, Remote: true,
		Template: "list at {task_list_path} / quoted {task_list_path_quoted}",
	}.Prompt()
	if !strings.Contains(got, url) {
		t.Errorf("{task_list_path} = %q, want the display address", got)
	}
	if strings.Contains(got, "gist://") {
		t.Errorf("a raw locator reached the agent: %q", got)
	}
	// The invariant the two placeholders are explained by.
	if !strings.Contains(got, domain.ShellQuote(url)) {
		t.Errorf("{task_list_path_quoted} must be ShellQuote of the same value: %q", got)
	}
}

func TestRemoteTaskManagementHintsDropThePathLine(t *testing.T) {
	const url = "https://gist.github.com/3f2a1b9c#file-brave-otter-md"

	local := domain.TaskManagementHints("brave-otter", "/home/me/tasks.md")
	if !strings.Contains(local, "--path") {
		t.Error("the local hints must keep the --path fallback")
	}

	remote := domain.RemoteTaskManagementHints("brave-otter", url)
	if strings.Contains(remote, "--path") {
		t.Errorf("the remote hints must not offer --path:\n%s", remote)
	}
	for _, want := range []string{"hap task brave-otter start", "hap task brave-otter done", "stored remotely"} {
		if !strings.Contains(remote, want) {
			t.Errorf("the remote hints must contain %q:\n%s", want, remote)
		}
	}
}
