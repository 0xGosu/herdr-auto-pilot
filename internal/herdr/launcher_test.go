package herdr

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/fakeherdr"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

var _ ports.AgentLauncher = (*CLI)(nil)

func launcherCLI(t *testing.T) (*CLI, *fakeherdr.FakeCLI) {
	t.Helper()
	fake, err := fakeherdr.NewFakeCLI(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &CLI{BinPath: fake.BinPath, Timeout: 5 * time.Second, startBusyDelay: time.Millisecond}, fake
}

func TestAgentByName(t *testing.T) {
	cli, fake := launcherCLI(t)
	ctx := context.Background()
	if _, ok, err := cli.AgentByName(ctx, "orchestrator"); err != nil || ok {
		t.Fatalf("unknown agent = %v, %v; want not found and no error", ok, err)
	}
	if err := fake.SetAgentGet(`{"id":"cli:agent:get","result":{"agent":{"agent":"claude","agent_status":"idle",` +
		`"pane_id":"w9:p1","tab_id":"w9:t1","terminal_id":"term_1","workspace_id":"w9"},"type":"agent_info"}}`); err != nil {
		t.Fatal(err)
	}
	a, ok, err := cli.AgentByName(ctx, "orchestrator")
	if err != nil || !ok || a.PaneID != "w9:p1" || a.TerminalID != "term_1" || a.AgentType != "claude" || a.Status != "idle" {
		t.Fatalf("AgentByName = %+v, %v, %v", a, ok, err)
	}
	if err := fake.SetFailing(true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := cli.AgentByName(ctx, "orchestrator"); err == nil {
		t.Fatal("a failing herdr must be an error, not 'no such agent' — that would spawn a duplicate")
	}
}

func TestNewPaneInWorkspaceReusesTheLabelledWorkspace(t *testing.T) {
	cli, fake := launcherCLI(t)
	ctx := context.Background()
	if err := fake.SetWorkspaceList(`{"result":{"workspaces":[{"workspace_id":"w1","label":"project","number":1}]}}`); err != nil {
		t.Fatal(err)
	}
	if err := fake.SetCreated(`{"result":{"root_pane":{"pane_id":"w5:p1"}}}`); err != nil {
		t.Fatal(err)
	}
	pane, err := cli.NewPaneInWorkspace(ctx, "hap-orchestrator", "orchestrator", "/state/orchestrator")
	if err != nil || pane != "w5:p1" {
		t.Fatalf("create = %q, %v", pane, err)
	}
	if err := fake.SetWorkspaceList(`{"result":{"workspaces":[{"workspace_id":"w5","label":"hap-orchestrator","number":5}]}}`); err != nil {
		t.Fatal(err)
	}
	if _, err := cli.NewPaneInWorkspace(ctx, "hap-orchestrator", "orchestrator", "/state/orchestrator"); err != nil {
		t.Fatal(err)
	}
	calls := strings.Join(fake.Calls(), "\n")
	if !strings.Contains(calls, "workspace create --cwd /state/orchestrator --label hap-orchestrator --no-focus") {
		t.Errorf("no workspace was created when none had the label:\n%s", calls)
	}
	if !strings.Contains(calls, "tab create --workspace w5 --cwd /state/orchestrator --label orchestrator --no-focus") {
		t.Errorf("the labelled workspace was not reused:\n%s", calls)
	}
}

func TestStartAgentRetriesABusyPane(t *testing.T) {
	cli, fake := launcherCLI(t)
	if err := fake.SetStartBusyOnce(); err != nil {
		t.Fatal(err)
	}
	if err := cli.StartAgent(context.Background(), "orchestrator", "claude", "w5:p1",
		[]string{"--model", "opus"}); err != nil {
		t.Fatalf("StartAgent = %v, want the busy refusal retried", err)
	}
	var starts []string
	for _, c := range fake.Calls() {
		if strings.HasPrefix(c, "agent start") {
			starts = append(starts, c)
		}
	}
	want := "agent start orchestrator --kind claude --pane w5:p1 --timeout 120000 -- --model opus"
	if len(starts) != 2 || starts[1] != want {
		t.Fatalf("agent start calls = %q, want two, the last %q", starts, want)
	}
}

func TestStartAgentDoesNotRetryOtherFailures(t *testing.T) {
	cli, fake := launcherCLI(t)
	if err := fake.SetFailing(true); err != nil {
		t.Fatal(err)
	}
	if err := cli.StartAgent(context.Background(), "orchestrator", "claude", "w5:p1", nil); err == nil {
		t.Fatal("StartAgent succeeded against a failing herdr")
	}
	n := 0
	for _, c := range fake.Calls() {
		if strings.HasPrefix(c, "agent start") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("a non-busy failure was retried %d times", n)
	}
}

// A success envelope with no pane is herdr reshaping its answer: reading it as
// "no such agent" would start a duplicate every retry.
func TestAgentByNameWithoutAPaneIsAnError(t *testing.T) {
	cli, fake := launcherCLI(t)
	if err := fake.SetAgentGet(`{"id":"cli:agent:get","result":{"agent":{}}}`); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := cli.AgentByName(context.Background(), "orchestrator"); err == nil || ok {
		t.Fatalf("AgentByName = %v, %v; want an error", ok, err)
	}
}

// A herdr that rejects the verbs (exit 2) says so distinctly, so the daemon
// stands down once instead of warning on every backoff step.
func TestLauncherReportsAnUnsupportedHerdr(t *testing.T) {
	cli, fake := launcherCLI(t)
	if err := os.WriteFile(fake.LegacyFlag, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, _, err := cli.AgentByName(ctx, "orchestrator"); !errors.Is(err, ports.ErrLaunchUnsupported) {
		t.Errorf("AgentByName = %v, want ErrLaunchUnsupported", err)
	}
	if err := cli.StartAgent(ctx, "orchestrator", "claude", "w5:p1", nil); !errors.Is(err, ports.ErrLaunchUnsupported) {
		t.Errorf("StartAgent = %v, want ErrLaunchUnsupported", err)
	}
}

func TestClosePane(t *testing.T) {
	cli, fake := launcherCLI(t)
	if err := cli.ClosePane(context.Background(), "w5:p1"); err != nil {
		t.Fatal(err)
	}
	if calls := fake.Calls(); len(calls) != 1 || calls[0] != "pane close w5:p1" {
		t.Fatalf("calls = %q", calls)
	}
}
