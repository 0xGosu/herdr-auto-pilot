package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// confirmSeam records what the operator-confirm seam was called with, and can
// be made to fail.
//
// It stands in for internal/frontend, which internal/daemon may not import.
// That is also this file's limit: a fake seam proves the EXECUTOR's guards, not
// the confirm's own behaviour, which is why the frontend package keeps driving
// ConfirmGeneratedTaskForOperator directly.
type confirmSeam struct {
	mu      sync.Mutex
	calls   []confirmCall
	err     error
	sendVia func(host ports.TaskSendHost) error
}

type confirmCall struct {
	auditID int64
	send    bool
	author  string
}

func (c *confirmSeam) confirm(ctx context.Context, auditID int64, send bool,
	author string, host ports.TaskSendHost) error {

	c.mu.Lock()
	c.calls = append(c.calls, confirmCall{auditID, send, author})
	fn, err := c.sendVia, c.err
	c.mu.Unlock()
	if fn != nil {
		if e := fn(host); e != nil {
			return e
		}
	}
	return err
}

func (c *confirmSeam) seen() []confirmCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]confirmCall(nil), c.calls...)
}

// newConfirmHarness is newAgentHarness with the operator-confirm seam wired
// BEFORE New(), so no daemon goroutine ever reads it mid-write.
func newConfirmHarness(t *testing.T, seam *confirmSeam, agentIDs ...string) *harness {
	t.Helper()
	rows := make([]domain.AgentTransition, 0, len(agentIDs))
	for _, id := range agentIDs {
		rows = append(rows, domain.AgentTransition{
			AgentID: id, PaneID: id, AgentType: "claude", Status: "idle",
		})
	}
	fl := &fakeLLM{}
	h := newHarnessCore(t, "", func(fh *fakeHerdr) ports.HerdrPort {
		fh.setAgents(rows)
		return fh
	}, fl, fl, nil, func(o *Options) {
		if seam != nil {
			o.ConfirmGeneratedTask = seam.confirm
		}
	})
	ctx := context.Background()
	for _, id := range agentIDs {
		if _, err := h.raw.EnsureAgentName(ctx, id); err != nil {
			t.Fatalf("ensure agent name %q: %v", id, err)
		}
	}
	h.waitForRoster(t, agentIDs...)
	return h
}

// seedGeneratedEscalation writes the row an operator confirms.
func seedGeneratedEscalation(t *testing.T, h *harness, agentID, tasks string) int64 {
	t.Helper()
	id, err := h.raw.AppendAudit(context.Background(), domain.AuditRecord{
		AgentID: agentID, AgentType: "claude", SituationType: domain.SituationIdle, Trigger: "t",
		Action: domain.AuditActionEscalated, Status: "escalated",
		Suggestion: domain.SuggestTaskPrefix + tasks, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func queueConfirm(t *testing.T, h *harness, agentID string, auditID int64, send bool) int64 {
	t.Helper()
	payload, err := json.Marshal(domain.AcceptGeneratedTaskPayload{AuditID: auditID, Send: send})
	if err != nil {
		t.Fatal(err)
	}
	return h.queueAction(domain.AgentAction{
		Kind: domain.AgentActionAcceptGeneratedTask, Target: agentID,
		Payload: string(payload), Author: "operator",
	})
}

// The happy path: the confirm reaches the seam with the OPERATOR's name on it.
//
// The author matters and is easy to lose. This runs inside the daemon, whose
// own front-end App is authored "daemon", so a seam that took the author from
// that App would attribute every remote operator's decision to the machine that
// executed it — and the correction this confirm writes is a learning event with
// a byline.
func TestAQueuedGeneratedTaskConfirmReachesTheSeam(t *testing.T) {
	seam := &confirmSeam{}
	h := newConfirmHarness(t, seam, "w1:p1")
	audit := seedGeneratedEscalation(t, h, "w1:p1", "write the parser")

	got := h.awaitAction(queueConfirm(t, h, "w1:p1", audit, true))
	if got.Status != domain.AgentActionDone {
		t.Fatalf("status = %q (%s), want done", got.Status, got.Error)
	}
	calls := seam.seen()
	if len(calls) != 1 {
		t.Fatalf("seam calls = %+v, want exactly 1", calls)
	}
	if calls[0].auditID != audit || !calls[0].send || calls[0].author != "operator" {
		t.Errorf("seam call = %+v, want audit %d, send, author \"operator\"", calls[0], audit)
	}
}

// A confirm whose escalation was dismissed while it sat in the queue must not
// write the list.
//
// The gap is real — the row travels a database push and pull for a remote
// operator — and acting anyway would create a task source and hand out a task
// for a suggestion a human has since withdrawn.
func TestAQueuedConfirmRefusesADismissedEscalation(t *testing.T) {
	seam := &confirmSeam{}
	h := newConfirmHarness(t, seam, "w1:p1")
	audit := seedGeneratedEscalation(t, h, "w1:p1", "write the parser")
	if err := h.raw.DismissEscalation(context.Background(), audit); err != nil {
		t.Fatal(err)
	}

	got := h.awaitAction(queueConfirm(t, h, "w1:p1", audit, true))
	if got.Status != domain.AgentActionFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if len(seam.seen()) != 0 {
		t.Errorf("the confirm ran for a dismissed escalation: %+v", seam.seen())
	}
}

// A row the auto-accept pass has already claimed is refused, and SAYS so.
//
// Both writers are legitimate and neither is a fault, but they must not both
// act: full self-prompting claims escalated -> auto_accepting and then calls
// the same confirm code directly. Answering with the generic "is %q" message
// would send an operator looking for a status nothing in the UI shows.
func TestAQueuedConfirmNamesAnFSPClaim(t *testing.T) {
	seam := &confirmSeam{}
	h := newConfirmHarness(t, seam, "w1:p1")
	audit := seedGeneratedEscalation(t, h, "w1:p1", "write the parser")
	if ok, err := h.raw.ClaimForAutoAccept(context.Background(), audit); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}

	got := h.awaitAction(queueConfirm(t, h, "w1:p1", audit, true))
	if got.Status != domain.AgentActionFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if !strings.Contains(got.Error, "already being accepted automatically") {
		t.Errorf("error = %q, want it to name the automatic acceptance", got.Error)
	}
	if len(seam.seen()) != 0 {
		t.Errorf("the confirm ran on a claimed row: %+v", seam.seen())
	}
}

// A confirm whose pane has been recycled under a different terminal is refused.
//
// Herdr reuses pane ids and an agent id IS a pane id, so between the operator's
// keypress and this drain the terminal behind it can be replaced — and the list
// would be built for one agent while the task was typed at another.
func TestAQueuedConfirmRefusesARecycledTerminal(t *testing.T) {
	seam := &confirmSeam{}
	h := newConfirmHarness(t, seam, "w1:p1")
	// The identity the daemon has actually observed. Without it both sides read
	// "not observed", which passes by design — only a positive MISMATCH refuses.
	if _, err := h.raw.SyncAgentTerminalID(context.Background(), "w1:p1", "term_new"); err != nil {
		t.Fatal(err)
	}
	audit := seedGeneratedEscalation(t, h, "w1:p1", "write the parser")

	payload, _ := json.Marshal(domain.AcceptGeneratedTaskPayload{AuditID: audit, Send: true})
	id := h.queueAction(domain.AgentAction{
		Kind: domain.AgentActionAcceptGeneratedTask, Target: "w1:p1",
		TerminalID: "term-from-another-life", Payload: string(payload), Author: "operator",
	})
	got := h.awaitAction(id)
	if got.Status != domain.AgentActionFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if len(seam.seen()) != 0 {
		t.Errorf("the confirm ran against a recycled pane: %+v", seam.seen())
	}
}

// An agent that went back to work refuses a confirm+send, carrying the marker
// that lets the surface offer "add to the list instead".
//
// The check lives here rather than in the confirm because this is the only
// process that can ask herdr anything. It bounds the SEND only.
func TestAQueuedConfirmRefusesABusyAgentOnlyWhenSending(t *testing.T) {
	seam := &confirmSeam{}
	h := newConfirmHarness(t, seam, "w1:p1")
	h.herdr.setAgents([]domain.AgentTransition{
		{AgentID: "w1:p1", PaneID: "w1:p1", AgentType: "claude", Status: "working"},
	})
	audit := seedGeneratedEscalation(t, h, "w1:p1", "write the parser")

	got := h.awaitAction(queueConfirm(t, h, "w1:p1", audit, true))
	if got.Status != domain.AgentActionFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if !strings.Contains(got.Error, domain.SuggestionStaleMarker) {
		t.Errorf("error = %q, want it to carry %q so the surface can offer the fallback",
			got.Error, domain.SuggestionStaleMarker)
	}
	if len(seam.seen()) != 0 {
		t.Errorf("nothing may be written for a stale suggestion: %+v", seam.seen())
	}

	// Add-only is fine while the agent works: the daemon delivers on its next
	// idle, and nothing touches the pane now.
	second := seedGeneratedEscalation(t, h, "w1:p1", "and the lexer")
	got = h.awaitAction(queueConfirm(t, h, "w1:p1", second, false))
	if got.Status != domain.AgentActionDone {
		t.Fatalf("add-only status = %q (%s), want done", got.Status, got.Error)
	}
	if len(seam.seen()) != 1 {
		t.Errorf("add-only confirm must still run: %+v", seam.seen())
	}
}

// side_effect is marked when the confirm SENDS, and only then.
//
// A row carrying it is FAILED at the next start rather than replayed, with "it
// may or may not have reached the agent". That is right after keystrokes and
// wrong before them: an add-only confirm types nothing and both its writes
// dedupe, so replaying it is harmless while failing it tells the operator
// something untrue about a task that was never sent.
func TestAQueuedConfirmMarksItsSideEffectOnlyWhenItSends(t *testing.T) {
	for _, tc := range []struct {
		name string
		send bool
		want bool
	}{
		{"a send marks it", true, true},
		{"add-only marks nothing", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seam := &confirmSeam{sendVia: func(host ports.TaskSendHost) error {
				if !tc.send {
					return nil
				}
				return host.Send(context.Background(), "w1:p1", "claude", "do the thing")
			}}
			h := newConfirmHarness(t, seam, "w1:p1")
			audit := seedGeneratedEscalation(t, h, "w1:p1", "write the parser")

			got := h.awaitAction(queueConfirm(t, h, "w1:p1", audit, tc.send))
			if got.Status != domain.AgentActionDone {
				t.Fatalf("status = %q (%s), want done", got.Status, got.Error)
			}
			if got.SideEffect != tc.want {
				t.Errorf("side_effect = %v, want %v", got.SideEffect, tc.want)
			}
		})
	}
}

// A build with no confirm seam refuses with the SHARED marker.
//
// The operator may be standing at a different machine than the daemon that
// refused, so the front end matches on this to re-phrase the advice with that
// node's label and version. Reporting "done" would be worse than any error: it
// tells them their tasks were written when nothing was.
func TestAQueuedConfirmWithNoSeamIsRefusedNotSilentlyDone(t *testing.T) {
	h := newConfirmHarness(t, nil, "w1:p1")
	audit := seedGeneratedEscalation(t, h, "w1:p1", "write the parser")

	got := h.awaitAction(queueConfirm(t, h, "w1:p1", audit, true))
	if got.Status != domain.AgentActionFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if !strings.Contains(got.Error, domain.ActionUnsupportedMarker) {
		t.Errorf("error = %q, want the shared unsupported marker", got.Error)
	}
}

// The seam's own failure reaches the operator verbatim.
func TestAQueuedConfirmReportsTheSeamsError(t *testing.T) {
	seam := &confirmSeam{err: errors.New("register task source: agent already has one")}
	h := newConfirmHarness(t, seam, "w1:p1")
	audit := seedGeneratedEscalation(t, h, "w1:p1", "write the parser")

	got := h.awaitAction(queueConfirm(t, h, "w1:p1", audit, false))
	if got.Status != domain.AgentActionFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if !strings.Contains(got.Error, "agent already has one") {
		t.Errorf("error = %q, want the seam's own sentence", got.Error)
	}
}

// A disabled agent suppresses the confirm, and says so in words an operator can
// act on.
//
// It runs inside the same per-agent lifecycle barrier every other delivery
// takes, so a disable landing after the guards above still stops it.
func TestAQueuedConfirmHonoursADisabledAgent(t *testing.T) {
	seam := &confirmSeam{}
	h := newConfirmHarness(t, seam, "w1:p1")
	if err := h.raw.SetAgentDisabled(context.Background(), "w1:p1", true); err != nil {
		t.Fatal(err)
	}
	audit := seedGeneratedEscalation(t, h, "w1:p1", "write the parser")

	got := h.awaitAction(queueConfirm(t, h, "w1:p1", audit, true))
	if got.Status != domain.AgentActionFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if !strings.Contains(got.Error, "disabled for automation") {
		t.Errorf("error = %q, want it to name the disable", got.Error)
	}
	if len(seam.seen()) != 0 {
		t.Errorf("the confirm ran for a disabled agent: %+v", seam.seen())
	}
}

// The host handed to FULL SELF-PROMPTING marks nothing.
//
// That path reaches the seam straight from the auto-accept sweep, not through
// the action queue, so there is no row to mark — and marking action 0 is a
// write against a row that does not exist, which would fail every unattended
// generated-task acceptance. Its own recovery is ReclaimAbandonedAutoAccepts on
// the audit row, which is why it needs none here.
func TestTheFSPTaskSendHostMarksNoSideEffect(t *testing.T) {
	h := newConfirmHarness(t, nil, "w1:p1")
	if err := h.daemon.taskSendHost(0).Send(context.Background(), "w1:p1", "claude", "do the thing"); err != nil {
		t.Fatalf("the FSP host must send without a queued row to mark, got %v", err)
	}
}
