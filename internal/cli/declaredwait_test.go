package cli_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/cli"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// #508 item 3: `hap wait` is the AGENT-facing verb of the per-agent switches.

func TestWaitDefaultsToThePaneItRunsIn(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	if _, err := st.EnsureAgentName(ctx, "w1:p9"); err != nil {
		t.Fatal(err)
	}
	// An agent id IS its herdr pane id, so the target comes from the
	// environment herdr set rather than from anything the agent wrote — which
	// is what stops an agent naming a sibling by mistake.
	t.Setenv("HERDR_PANE_ID", "w1:p9")

	var out bytes.Buffer
	if err := cli.Run(ctx, app, &out, "wait", []string{"20m", "--reason", "cold native build"}); err != nil {
		t.Fatalf("hap wait: %v", err)
	}

	w, err := st.AgentWaitFor(ctx, "w1:p9")
	if err != nil {
		t.Fatal(err)
	}
	if !w.Active(time.Now()) {
		t.Fatal("hap wait recorded no standing wait")
	}
	if w.Reason != "cold native build" {
		t.Errorf("reason = %q", w.Reason)
	}
	if !strings.Contains(out.String(), "waiting for 20m") {
		t.Errorf("output did not name the wait:\n%s", out.String())
	}

	// --clear is how an agent that finished early ends it.
	out.Reset()
	if err := cli.Run(ctx, app, &out, "wait", []string{"--clear"}); err != nil {
		t.Fatalf("hap wait --clear: %v", err)
	}
	if w, err = st.AgentWaitFor(ctx, "w1:p9"); err != nil {
		t.Fatal(err)
	}
	if w.Active(time.Now()) {
		t.Fatal("--clear left a standing wait")
	}
}

func TestWaitTakesAnExplicitAgent(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	if _, err := st.EnsureAgentName(ctx, "w1:p4"); err != nil {
		t.Fatal(err)
	}
	if err := st.RenameAgent(ctx, "w1:p4", "vivid-falcon"); err != nil {
		t.Fatal(err)
	}
	// Set a DIFFERENT pane in the environment, so a verb that ignored --agent
	// would declare for the wrong one rather than fail.
	t.Setenv("HERDR_PANE_ID", "w1:p99")

	var out bytes.Buffer
	if err := cli.Run(ctx, app, &out, "wait", []string{"--agent", "vivid-falcon", "30m"}); err != nil {
		t.Fatalf("hap wait: %v", err)
	}
	w, err := st.AgentWaitFor(ctx, "w1:p4")
	if err != nil {
		t.Fatal(err)
	}
	if !w.Active(time.Now()) {
		t.Fatal("--agent did not reach the named agent")
	}
}

// TestWaitRefusesWhatItCannotBound is the safety half: an unbounded wait is
// `hap disable` by another name, minus every place that says so, and a
// unit-less number is the operator mistake that silently becomes nanoseconds.
func TestWaitRefusesWhatItCannotBound(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	if _, err := st.EnsureAgentName(ctx, "w1:p7"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_PANE_ID", "w1:p7")

	for _, args := range [][]string{
		{"20"},             // no unit
		{"3h"},             // past MaxDeclaredWait
		{"30s"},            // below MinDeclaredWait
		{},                 // no duration at all
		{"--clear", "20m"}, // both spellings at once
	} {
		var out bytes.Buffer
		if err := cli.Run(ctx, app, &out, "wait", args); err == nil {
			t.Fatalf("hap wait %v was accepted", args)
		}
		w, err := st.AgentWaitFor(ctx, "w1:p7")
		if err != nil {
			t.Fatal(err)
		}
		if !w.Until.IsZero() {
			t.Fatalf("hap wait %v was refused but still wrote a row", args)
		}
	}
}

// TestWaitNeedsATargetWhenThereIsNoPane covers the one case the default cannot
// answer: run outside a herdr pane, it must say so rather than guess.
func TestWaitNeedsATargetWhenThereIsNoPane(t *testing.T) {
	app, _ := testApp(t)
	t.Setenv("HERDR_PANE_ID", "")
	var out bytes.Buffer
	err := cli.Run(context.Background(), app, &out, "wait", []string{"20m"})
	if err == nil {
		t.Fatal("hap wait outside a pane was accepted")
	}
	if !strings.Contains(err.Error(), "--agent") {
		t.Errorf("the refusal must name the remedy, got: %v", err)
	}
}

// TestAgentsListsAWaitingAgent pins the third value in the automation COLUMN
// rather than a new column: the row is tab-separated and scripts parse it by
// field number, so a new column would shift cwd, mode and node for every
// existing reader.
func TestAgentsListsAWaitingAgent(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	if err := st.PublishRoster(ctx, []domain.RosterAgent{{
		AgentID: "w1:p3", PaneID: "w1:p3", AgentType: "claude", Status: "idle", SeenAt: time.Now(),
	}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsureAgentName(ctx, "w1:p3"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetAgentWait(ctx, "w1:p3", time.Now().Add(14*time.Minute), "cold build"); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := cli.Run(ctx, app, &out, "agents", nil); err != nil {
		t.Fatal(err)
	}
	line := ""
	for _, l := range strings.Split(out.String(), "\n") {
		if strings.Contains(l, "w1:p3") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("the agent is missing from the listing:\n%s", out.String())
	}
	fields := strings.Split(line, "\t")
	if len(fields) < 5 {
		t.Fatalf("row has %d fields: %q", len(fields), line)
	}
	if !strings.HasPrefix(fields[4], "waiting ") {
		t.Errorf("automation column = %q, want a waiting state", fields[4])
	}
}
