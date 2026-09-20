package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// #526 item 3. A `hap resolve <id> --action "<review fix instructions>" --send`
// from the orchestrator was refused because the phrase "without deleting the PK
// row" was followed by the word "table": the suspected-irreversible heuristic
// corroborates a destructive verb against a data target across a line or two,
// which is right for a PENDING PANE OPERATION and wrong for prose instructing an
// agent. Nothing was sent, the refusal is final for that item, and the pull
// request it answered stalled about three hours until a human stepped in.
//
// This is the exact text shape, kept as one string so both arms below screen
// byte-identical prose — the difference under test is the AUTHOR, nothing else.
const reviewFixProse = "Fix the review comment on the migration: rewrite it to drop only the " +
	"unique index, without deleting the PK row.\n" +
	"The table must keep its existing constraint."

// deliverReplyAs queues a reply authored by author and waits for the verdict.
func (h *harness) deliverReplyAs(author string, auditID int64, action, paneID string) domain.AgentAction {
	h.t.Helper()
	payload, err := json.Marshal(domain.DeliverReplyPayload{AuditID: auditID, Action: action})
	if err != nil {
		h.t.Fatal(err)
	}
	id := h.queueAction(domain.AgentAction{
		Kind: domain.AgentActionDeliverReply, Target: paneID, Payload: string(payload),
		Author: author, CreatedAt: time.Now(),
	})
	return h.awaitAction(id)
}

// idleReplyEscalation seeds an idle escalation, the shape a free-text `--action`
// answers: a menu reply would be mapped to a digit and never carry prose.
func (h *harness) idleReplyEscalation(agentID string) int64 {
	h.t.Helper()
	return h.seedEscalation(domain.AuditRecord{
		AgentID: agentID, SituationType: domain.SituationIdle,
	})
}

func TestOrchestratorProseIsNotRefusedByThePaneHeuristic(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setPane("Waiting for your next instruction.\n")
	id := h.idleReplyEscalation("a1")

	got := h.deliverReplyAs(domain.OrchestratorAuthor, id, reviewFixProse, "a1")
	if got.Status != domain.AgentActionDone {
		t.Fatalf("status = %q (%s), want done", got.Status, got.Error)
	}
	if in := h.herdr.sentInputs(); len(in) != 1 || !strings.Contains(in[0], "unique index") {
		t.Fatalf("sent %v, want the review-fix prose", in)
	}
}

// TestTheOperatorKeepsTheIrreversibleHeuristic is the control, and without it
// the case above passes on a build that screens nobody. The operator's arm is
// deliberately unchanged: screenOutbound, heuristic included.
func TestTheOperatorKeepsTheIrreversibleHeuristic(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setPane("Waiting for your next instruction.\n")
	id := h.idleReplyEscalation("a1")

	got := h.deliverReplyAs(domain.OperatorAuthor, id, reviewFixProse, "a1")
	if got.Status == domain.AgentActionDone {
		t.Fatalf("the operator's arm must still run the heuristic; status = %q", got.Status)
	}
	if in := h.herdr.sentInputs(); len(in) != 0 {
		t.Fatalf("a refused reply must type nothing, sent %v", in)
	}
}

// TestOrchestratorProseStillMeetsTheStrictRules pins what is NOT dropped: the
// whole of the operator's declared policy is strict-kind, and so are the shipped
// literal command shapes. Strict-only is a narrower screen, not an absent one.
func TestOrchestratorProseStillMeetsTheStrictRules(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setPane("Waiting for your next instruction.\n")
	id := h.idleReplyEscalation("a1")

	got := h.deliverReplyAs(domain.OrchestratorAuthor, id,
		"Clean the worktree with rm -rf ~ and start again", "a1")
	if got.Status == domain.AgentActionDone {
		t.Fatalf("a strict seed must still refuse the orchestrator; status = %q", got.Status)
	}
	if in := h.herdr.sentInputs(); len(in) != 0 {
		t.Fatalf("a refused reply must type nothing, sent %v", in)
	}
}

// TestOrchestratorReplyStillMeetsTheActionRules is the difference between a
// reply and a task hand-out, and the reason deliverReplyScreen is not simply
// actionScreen's send_task arm. A hand-out is prose by construction; a reply is
// routinely the LABEL of an option on a live menu, so a widening choice picked
// by an LLM is exactly what the action rules exist to refuse.
func TestOrchestratorReplyStillMeetsTheActionRules(t *testing.T) {
	// The shipped action rules are opt-in (see SeedNeverAutoActionRules), so the
	// key is what makes this case test the rules rather than their absence.
	h := newHarness(t, "[safety]\nenable_never_auto_action_seeds = true\n")
	h.herdr.setPane("Do you want to proceed?\n" +
		"❯ 1. Yes, run command\n" +
		"  2. Yes, and always allow commands starting with go test\n" +
		"  3. No\n")
	id := h.seedEscalation(domain.AuditRecord{
		AgentID: "a1", SituationType: domain.SituationApproval,
	})

	got := h.deliverReplyAs(domain.OrchestratorAuthor, id,
		"Yes, and always allow commands starting with go test", "a1")
	if got.Status == domain.AgentActionDone {
		t.Fatalf("a widening option must still be refused; status = %q", got.Status)
	}
	if in := h.herdr.sentInputs(); len(in) != 0 {
		t.Fatalf("a refused reply must type nothing, sent %v", in)
	}
}

// TestARefusedReplyReachesTheOperator covers item 3's second half. Before it, a
// refusal set agent_actions.error, deleted the correction and logged one line —
// nothing in audit_log, nothing in the queue, and the only party told was the
// process that queued the action, which for the orchestrator is its own
// terminal that has just been told the refusal is final.
func TestARefusedReplyReachesTheOperator(t *testing.T) {
	h := newHarness(t, "")
	ctx := context.Background()
	h.herdr.setPane("Waiting for your next instruction.\n")
	id := h.idleReplyEscalation("a1")

	got := h.deliverReplyAs(domain.OperatorAuthor, id, reviewFixProse, "a1")
	if got.Status == domain.AgentActionDone {
		t.Fatalf("the reply should have been refused; status = %q", got.Status)
	}

	rows := rationalesContaining(t, h, "["+domain.ReasonQueuedActionRefused+"]")
	if len(rows) != 1 {
		t.Fatalf("a refusal must raise exactly one escalation, got %d", len(rows))
	}
	if !strings.Contains(rows[0], "nothing was sent") {
		t.Fatalf("the rationale must say nothing was sent: %q", rows[0])
	}

	// It is informational: no suggestion, so nothing can confirm it into the
	// pane — the text a safety control just refused is exactly what a confirm
	// would type.
	pending, err := h.raw.PendingEscalations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range pending {
		if !strings.Contains(r.Rationale, "["+domain.ReasonQueuedActionRefused+"]") {
			continue
		}
		found = true
		if r.Suggestion != "" {
			t.Fatalf("a refusal row must carry no suggestion, got %q", r.Suggestion)
		}
	}
	if !found {
		t.Fatal("the refusal escalation is not in the operator's queue")
	}
}

// TestAClosedEscalationRaisesNoRefusalRow is the scope control for the row
// above. withdrawsCorrection also covers errEscalationClosed — the row this
// answered is already resolved or dismissed — and that is the opposite
// situation: there is nothing left to tell anyone about.
func TestAClosedEscalationRaisesNoRefusalRow(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setPane("Waiting for your next instruction.\n")
	id := h.seedEscalation(domain.AuditRecord{
		AgentID: "a1", SituationType: domain.SituationIdle, Status: "dismissed",
	})

	got := h.deliverReplyAs(domain.OperatorAuthor, id, "continue", "a1")
	if got.Status == domain.AgentActionDone {
		t.Fatalf("a dismissed escalation must not be answered; status = %q", got.Status)
	}
	if rows := rationalesContaining(t, h, "["+domain.ReasonQueuedActionRefused+"]"); len(rows) != 0 {
		t.Fatalf("a closed escalation must raise no refusal row, got %d", len(rows))
	}
}

// TestARefusedHandOutIsFiledUnderTheAgentID is the case a reply-only suite
// cannot reach, and the reason escalateRefusedAction resolves its target.
//
// `send_task` queues the agent's NAME, not its id (frontend.SendTaskToAgentOn),
// while `deliver_reply` and `accept_generated_task` both queue an audit row's
// AgentID. Filing the refusal under the name would write an audit_log.agent_id
// that joins to nothing in agent_names — invisible to every per-agent query, on
// precisely the kind whose refusal has no other row at all.
func TestARefusedHandOutIsFiledUnderTheAgentID(t *testing.T) {
	// The shared sendTaskSeam only RECORDS the screen closure; the real seam
	// calls it right before the send. This one does too, or the refusal under
	// test never happens and the case passes for the wrong reason.
	seam := &sendTaskSeam{}
	seam.run = func(p domain.SendTaskPayload, screen func(string) error) error {
		if screen == nil {
			return nil
		}
		return screen(p.TaskText)
	}
	h := newSendTaskHarnessWith(t, seam,
		"[safety]\nnever_auto_patterns = [\"wipe the staging bucket\"]\n", "w1:p4")
	ctx := context.Background()

	name, err := h.raw.EnsureAgentName(ctx, "w1:p4")
	if err != nil {
		t.Fatal(err)
	}
	if name == "w1:p4" {
		t.Fatal("the harness must give the agent a short name distinct from its id")
	}

	payload, err := json.Marshal(domain.SendTaskPayload{
		Locator: "/tmp/tasks.md", Index: 1, TaskText: "wipe the staging bucket",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Queued by NAME, as the real hand-out path does, and authored by the
	// orchestrator so the text is screened at all.
	id := h.queueAction(domain.AgentAction{
		Kind: domain.AgentActionSendTask, Target: name,
		Payload: string(payload), Author: domain.OrchestratorAuthor,
		CreatedAt: time.Now(),
	})
	if got := h.awaitAction(id); got.Status == domain.AgentActionDone {
		t.Fatalf("a never-auto match must refuse the hand-out; status = %q", got.Status)
	}

	rows, err := h.raw.PendingEscalations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rows {
		if !strings.Contains(r.Rationale, "["+domain.ReasonQueuedActionRefused+"]") {
			continue
		}
		found = true
		if r.AgentID != "w1:p4" {
			t.Fatalf("the refusal is filed under %q, want the agent id w1:p4", r.AgentID)
		}
	}
	if !found {
		t.Fatal("a refused hand-out raised no escalation")
	}
}
