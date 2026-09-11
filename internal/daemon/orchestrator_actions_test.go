package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// The orchestrator's hand-out is an LLM's: the seam is handed the daemon's
// outbound screen, and the herd's pause refuses it. An operator's gets neither
// — the control that proves both are keyed on the author.
func TestOrchestratorActionsAreScreenedAndPaused(t *testing.T) {
	seam := &sendTaskSeam{}
	h := newSendTaskHarness(t, seam, "w1:p1")
	ctx := context.Background()
	queue := func(author string) domain.AgentAction {
		t.Helper()
		payload, err := json.Marshal(domain.SendTaskPayload{Locator: "/tmp/tasks.md", Index: 1, TaskText: "work"})
		if err != nil {
			t.Fatal(err)
		}
		return h.awaitAction(h.queueAction(domain.AgentAction{
			Kind: domain.AgentActionSendTask, Target: "w1:p1", Payload: string(payload), Author: author,
		}))
	}

	if got := queue("operator"); got.Status != domain.AgentActionDone {
		t.Fatalf("operator hand-out: %q (%s)", got.Status, got.Error)
	}
	if got := queue(domain.OrchestratorAuthor); got.Status != domain.AgentActionDone {
		t.Fatalf("orchestrator hand-out: %q (%s)", got.Status, got.Error)
	}
	calls := seam.seen()
	if len(calls) != 2 || calls[0].screen != nil || calls[1].screen == nil {
		t.Fatalf("%d calls; want the operator's with no screen and the orchestrator's with one", len(calls))
	}
	if err := calls[1].screen("Run rm -rf ~ to wipe the home directory?"); !errors.Is(err, errOutboundRefused) {
		t.Errorf("the orchestrator's screen let a home-directory wipe through: %v", err)
	}
	if err := calls[1].screen("Your next task is: write the release notes."); err != nil {
		t.Errorf("the orchestrator's screen refused an ordinary task: %v", err)
	}

	if _, err := h.raw.InsertKillEvent(ctx, domain.KillEvent{State: domain.KillStateActiveValue,
		Scope: domain.KillScopeGlobal, Author: "operator", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if got := queue(domain.OrchestratorAuthor); got.Status == domain.AgentActionDone ||
		!strings.Contains(got.Error, "paused") {
		t.Fatalf("a paused herd ran the orchestrator's hand-out: %q (%s)", got.Status, got.Error)
	}
	if got := queue("operator"); got.Status != domain.AgentActionDone {
		t.Fatalf("the pause refused the OPERATOR's hand-out: %q (%s)", got.Status, got.Error)
	}
}

// The orchestrator's confirm of a generated-task suggestion is handed the
// screen too; the operator's is not.
func TestOrchestratorGeneratedTaskConfirmIsScreened(t *testing.T) {
	seam := &confirmSeam{}
	h := newConfirmHarness(t, seam, "w1:p1")
	for _, author := range []string{"operator", domain.OrchestratorAuthor} {
		audit := seedGeneratedEscalation(t, h, "w1:p1", "write the parser")
		payload, err := json.Marshal(domain.AcceptGeneratedTaskPayload{AuditID: audit, Send: false})
		if err != nil {
			t.Fatal(err)
		}
		got := h.awaitAction(h.queueAction(domain.AgentAction{
			Kind: domain.AgentActionAcceptGeneratedTask, Target: "w1:p1", Payload: string(payload), Author: author,
		}))
		if got.Status != domain.AgentActionDone {
			t.Fatalf("%s confirm: %q (%s)", author, got.Status, got.Error)
		}
	}
	calls := seam.seen()
	if len(calls) != 2 || calls[0].screened || !calls[1].screened {
		t.Fatalf("calls = %+v; want the operator's unscreened and the orchestrator's screened", calls)
	}
}

// Another session took the name "orchestrator" between the lookup and the
// start: herdr refused ours, and the re-probe finds THEIRS. It must not be
// recorded, disabled or briefed, and our pane is closed.
func TestOrchestratorNameTakenDuringTheStartIsNotAdopted(t *testing.T) {
	h, l, _ := newOrchHarness(t, "", nil)
	cfg := orchestratorModeOnIn(h)
	other := domain.AgentTransition{AgentID: "wZ:p4", PaneID: "wZ:p4", AgentType: "claude",
		Status: "idle", TerminalID: "term_other"}
	l.failStart = true
	l.onStart = func() {
		l.lmu.Lock()
		l.named[domain.OrchestratorAgentName] = other
		l.lmu.Unlock()
	}
	h.daemon.ensureOrchestrator(context.Background(), l, cfg, nil)

	if id := h.daemon.orchestratorIdentity(); id.Known() {
		t.Fatalf("recorded %+v as the orchestrator", id)
	}
	if closed := l.closedPanes(); len(closed) != 1 || closed[0] != "wO:p1" {
		t.Fatalf("closed panes = %q, want our own wO:p1", closed)
	}
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Fatalf("typed into a session hap did not start: %q", got)
	}
	if disabled, _ := h.raw.AgentDisabled(context.Background(), "wZ:p4"); disabled {
		t.Fatal("disabled a session hap did not start")
	}
}

// A hap command knows it runs inside the orchestrator by its herdr pane.
func TestCallerIsOrchestrator(t *testing.T) {
	h, _, state := newOrchHarness(t, "", nil)
	if CallerIsOrchestrator(state, "wO:p9") {
		t.Fatal("no identity recorded, yet a pane was taken for the orchestrator")
	}
	h.daemon.setOrchestratorIdentity(domain.OrchestratorIdentity{PaneID: "wO:p9", TerminalID: "term_9"})
	if !CallerIsOrchestrator(state, "wO:p9") {
		t.Fatal("the orchestrator's own pane was not recognized")
	}
	if CallerIsOrchestrator(state, "wO:p1") || CallerIsOrchestrator(state, "") {
		t.Fatal("another pane, or no pane, was taken for the orchestrator")
	}
}
