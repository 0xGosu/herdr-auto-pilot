package daemon

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// The post-consult staleness re-check had three possible outcomes and no
// coverage at all: every re-read that did not match escalated, so a prompt the
// operator had already answered by hand came back as a queue entry pointing at
// a screen that no longer existed.
//
// These cases are agy because agy is the only agent type the "gone" answer can
// currently reach. Classify returns SituationIdle only for an idle/done agent,
// and the re-check re-classifies against the situation's ORIGINAL status, which
// for a claude or codex modal is "blocked" — such a vanished prompt re-reads as
// unclassifiable and still escalates. agy reports every modal as idle/done, so
// its vanished prompt re-reads as idle. That is also where the ghosts were
// actually observed.
const staleConsultLLM = "[llm]\ncommand = [\"fake\"]\ntimeout_seconds = 5\n" +
	"auto_act_confidence_threshold = 50\n"

// agyOtherApprovalPane is a DIFFERENT agy approval: same shape, different
// command. A re-read landing here is a situation that genuinely CHANGED, which
// must still reach the operator — it is not the same question, and answering it
// with the previous consult's verdict would approve a command nobody weighed.
var agyOtherApprovalPane = strings.ReplaceAll(agyApprovalPane, "go test ./...", "go build ./...")

// staleDismissals returns the audit rows carrying the machine's stale-dismissal
// tag, which is what separates "recorded and closed" from "queued for a human".
func staleDismissals(t *testing.T, h *harness) []domain.AuditRecord {
	t.Helper()
	rows, err := h.raw.AuditLog(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	var out []domain.AuditRecord
	for _, r := range rows {
		if domain.AutoDismissReason(r.Rationale) == domain.ReasonAutoDismissStale {
			out = append(out, r)
		}
	}
	return out
}

// TestConsultAnswerIsDeliveredWhenTheSituationHeldStill is the control for the
// two dismissal cases below. Without it, a change that simply stopped
// delivering post-consult answers would leave both of them green: "nothing was
// typed" is exactly what a broken send path also produces.
func TestConsultAnswerIsDeliveredWhenTheSituationHeldStill(t *testing.T) {
	h := newHarnessConsult(t, staleConsultLLM,
		func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
			return &domain.LLMDecision{Action: "Yes, run command", ConfidentScore: 99}, nil
		})
	h.herdr.setKeyScript(agyApprovalPane, []string{"1"}, []string{agyReadyPane})

	h.pushAgy("agent-stale-hold", "done")

	if keys := waitForKeys(t, h, 1); !reflect.DeepEqual(keys, []string{"1"}) {
		t.Fatalf("keys = %v, want exactly [1]", keys)
	}
	if pend, _ := h.raw.PendingEscalations(context.Background()); len(pend) != 0 {
		t.Errorf("a still-current situation must be answered, not escalated: %+v", pend)
	}
}

// TestAStaleConsultWhosePromptIsGoneIsDismissedNotEscalated: the approval was
// answered by hand while the consult ran, so there is no question left to ask.
// Recorded as dismissed and kept OUT of the operator's queue — a "dismissal"
// the operator still has to open fixes nothing.
func TestAStaleConsultWhosePromptIsGoneIsDismissedNotEscalated(t *testing.T) {
	var h *harness
	h = newHarnessConsult(t, staleConsultLLM,
		func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
			h.herdr.setPane(agyReadyPane)
			return &domain.LLMDecision{Action: "Yes, run command", ConfidentScore: 99}, nil
		})
	h.herdr.setPane(agyApprovalPane)
	ctx := context.Background()

	h.pushAgy("agent-stale-gone", "done")

	var rec domain.AuditRecord
	waitFor(t, 5*time.Second, func() bool {
		got := staleDismissals(t, h)
		if len(got) == 0 {
			return false
		}
		rec = got[0]
		return true
	})
	if rec.Status != "dismissed" {
		t.Errorf("status = %q, want dismissed", rec.Status)
	}
	if pend, _ := h.raw.PendingEscalations(ctx); len(pend) != 0 {
		t.Errorf("a ghost must not reach the operator queue: %+v", pend)
	}
	if keys := h.herdr.keysSent(); len(keys) != 0 {
		t.Errorf("nothing may be typed at a pane whose prompt is gone, got %v", keys)
	}
}

// TestAStaleConsultWhoseSituationChangedStillEscalates: the pane moved to a
// DIFFERENT approval. That is the case the escalation exists for, and the
// dismissal must not swallow it.
func TestAStaleConsultWhoseSituationChangedStillEscalates(t *testing.T) {
	var h *harness
	h = newHarnessConsult(t, staleConsultLLM,
		func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
			h.herdr.setPane(agyOtherApprovalPane)
			return &domain.LLMDecision{Action: "Yes, run command", ConfidentScore: 99}, nil
		})
	h.herdr.setPane(agyApprovalPane)

	// The fixture must genuinely be a DIFFERENT situation. This case would pass
	// on the old code too, which escalated unconditionally, so without this the
	// whole test could be asserting on a pane that never actually moved — the
	// two approvals differ only in the command, and their option lists are
	// byte-identical.
	cl := classifierForTest()
	before := domain.ComputeSignature(cl.Classify(domain.AgentTypeAgy, "done", agyApprovalPane))
	after := domain.ComputeSignature(cl.Classify(domain.AgentTypeAgy, "done", agyOtherApprovalPane))
	if before.Raw == after.Raw {
		t.Fatalf("fixture is not a different situation: both panes hash to %s", before.Raw)
	}

	h.pushAgy("agent-stale-changed", "done")

	esc := waitForOneEscalation(t, h)
	if !strings.Contains(esc.Rationale, "stale") {
		t.Errorf("rationale = %q, want the staleness reason", esc.Rationale)
	}
	if got := staleDismissals(t, h); len(got) != 0 {
		t.Errorf("a situation that CHANGED must be asked about, not dismissed: %+v", got)
	}
	if keys := h.herdr.keysSent(); len(keys) != 0 {
		t.Errorf("the decided form is no longer on screen, so nothing may be typed, got %v", keys)
	}
}

// TestAStaleConsultOnAnUnreadyAgyComposerStillEscalates is the case that
// discriminates the whole design, and the reason "the re-read classified as
// idle" is not sufficient evidence on its own.
//
// herdr reports every agy modal as idle/done, so an agy pane reading "idle"
// says nothing about whether something is still standing on it. Here the
// composer is not at rest, the re-read classifies as idle anyway, and the
// outcome must be an escalation: dismissing it would silently discard exactly
// the stuck-modal case hap is least able to recognize.
func TestAStaleConsultOnAnUnreadyAgyComposerStillEscalates(t *testing.T) {
	var h *harness
	h = newHarnessConsult(t, staleConsultLLM,
		func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
			h.herdr.setPane(agyDraftPane)
			return &domain.LLMDecision{Action: "Yes, run command", ConfidentScore: 99}, nil
		})
	h.herdr.setPane(agyApprovalPane)

	h.pushAgy("agent-stale-unready", "done")

	esc := waitForOneEscalation(t, h)
	if !strings.Contains(esc.Rationale, "stale") {
		t.Errorf("rationale = %q, want the staleness reason", esc.Rationale)
	}
	if got := staleDismissals(t, h); len(got) != 0 {
		t.Errorf("an agy pane reading idle under an unready composer is not proof the "+
			"prompt is gone; it must escalate: %+v", got)
	}
	if keys := h.herdr.keysSent(); len(keys) != 0 {
		t.Errorf("nothing may be typed while the composer is not at rest, got %v", keys)
	}
}

// TestConsultTargetGoneRequiresMoreThanIdleOnAgy pins the predicate itself. The
// daemon cases above each drive one branch through the whole pipeline; this
// asserts the rule directly, so a widening that happens to leave those green
// (a different pane fixture, a changed classifier) still fails here.
func TestConsultTargetGoneRequiresMoreThanIdleOnAgy(t *testing.T) {
	idle := domain.Situation{Type: domain.SituationIdle}
	approval := domain.Situation{Type: domain.SituationApproval}
	unclassifiable := domain.Situation{Type: domain.SituationUnclassifiable}

	cases := []struct {
		name      string
		agentType string
		current   domain.Situation
		pane      string
		want      bool
	}{
		{"agy at rest is gone", domain.AgentTypeAgy, idle, agyReadyPane, true},
		{"agy holding a draft is not", domain.AgentTypeAgy, idle, agyDraftPane, false},
		{"agy under a standing approval is not", domain.AgentTypeAgy, idle, agyApprovalPane, false},
		{"a classified approval is never gone", domain.AgentTypeAgy, approval, agyReadyPane, false},
		// Classify's DEFAULT. Reading "we could not make sense of this" as
		// "nothing is there" would dismiss the screens hap understands least.
		{"unclassifiable is never gone", domain.AgentTypeAgy, unclassifiable, agyReadyPane, false},
		{"claude idle needs no composer proof", "claude", idle, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := domain.ConsultTargetGone(tc.agentType, tc.current, tc.pane); got != tc.want {
				t.Errorf("ConsultTargetGone = %v, want %v", got, tc.want)
			}
		})
	}
}
