package domain_test

import (
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// TestRemotePromptDropsThePathFallback is deliberately a literal string
// assertion: neither default may mention `--path` — it always reads a LOCAL
// file, so as a fallback it is dead advice under a remote provider. The
// fallback both defaults offer is the task-source INDEX, which addresses the
// source through hap's own config on every provider.
func TestRemotePromptDropsThePathFallback(t *testing.T) {
	const url = "https://gist.github.com/3f2a1b9c#file-brave-otter-md"

	local := domain.DeclaredTask{
		Task: "step two", Path: "/home/me/tasks.md", AgentName: "brave-otter", SourceIndex: "1",
	}.Prompt()
	if strings.Contains(local, "--path") {
		t.Errorf("the LOCAL default must not offer --path — its fallback is the source index:\n%s", local)
	}
	if !strings.Contains(local, "use the task-source index `1`") {
		t.Errorf("the local default must offer the source-index fallback:\n%s", local)
	}

	remote := domain.DeclaredTask{
		Task: "step two", Path: url, AgentName: "brave-otter", Remote: true, SourceIndex: "1",
	}.Prompt()
	if strings.Contains(remote, "--path") {
		t.Errorf("the remote default must not offer --path:\n%s", remote)
	}
	if !strings.Contains(remote, "use the task-source index `1`") {
		t.Errorf("the remote default must offer the source-index fallback — a shared remote list is exactly the source a name cannot address:\n%s", remote)
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

	local := domain.TaskManagementHints("brave-otter", "/home/me/tasks.md", "1")
	if strings.Contains(local, "--path") {
		t.Errorf("the local hints must not offer --path — the fallback is the source index:\n%s", local)
	}
	if !strings.Contains(local, "task-source index `1`") {
		t.Errorf("the local hints must offer the source-index fallback:\n%s", local)
	}

	remote := domain.RemoteTaskManagementHints("brave-otter", url, "1")
	if strings.Contains(remote, "--path") {
		t.Errorf("the remote hints must not offer --path:\n%s", remote)
	}
	if !strings.Contains(remote, "task-source index `1`") {
		t.Errorf("the remote hints must offer the source-index fallback — a shared remote list is exactly the source a name cannot address:\n%s", remote)
	}
	for _, want := range []string{"hap task brave-otter start", "hap task brave-otter done", "stored remotely"} {
		if !strings.Contains(remote, want) {
			t.Errorf("the remote hints must contain %q:\n%s", want, remote)
		}
	}
}
