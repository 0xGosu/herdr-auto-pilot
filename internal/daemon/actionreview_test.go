package daemon

// Tests for the optional outbound action review (llm.enable_rewrite_action):
// the async consult pipeline in startActionReview/handleActionReviewOutcome,
// its safety re-gates, and the never-block-the-send fallback semantics.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// reviewCfg builds the [llm] block that enables the action review; extraLLM
// keys are appended INSIDE the [llm] table (no new header).
func reviewCfg(extraLLM string) string {
	return "[llm]\ncommand = [\"fake\"]\nenable_rewrite_action = true\ntimeout_seconds = 5\n" + extraLLM
}

const freeApprovalPane = "Do you want to continue? (y/n)\n"

// approvalReviewHarness seeds a free-text approval rule ("y") for the given
// agent — a send the action review applies to (no numbered menu, no declared
// task) — and configures the fake consult LLM.
func approvalReviewHarness(t *testing.T, extraLLM string) *harness {
	t.Helper()
	h := newHarness(t, reviewCfg(extraLLM))
	h.herdr.setPane(freeApprovalPane)
	h.seedAutonomous(freeApprovalPane, domain.SituationApproval, "y")
	h.llm.configured = true
	return h
}

// respondReview installs a consult fake that asserts the request is an
// action review, stages the decision row (as submit_decision would), and
// returns it. calls counts consults; a nil answer simulates "no submission".
func respondReview(h *harness, calls *atomic.Int32, score int, answer func(req domain.LLMRequest) (string, error)) {
	h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		calls.Add(1)
		if !req.ActionReview || req.ProposedAction == "" {
			return nil, errors.New("expected an action-review consult with a proposed action")
		}
		if req.First {
			return nil, errors.New("action reviews must not consume the first-consult priming")
		}
		action, err := answer(req)
		if err != nil {
			return nil, err
		}
		id, ierr := h.raw.InsertLLMDecision(ctx, domain.LLMDecision{
			RequestID: req.RequestID, Signature: req.Signature,
			SituationType: req.SituationType, AgentType: req.AgentType,
			Action: action, Rationale: "review rationale", ConfidentScore: score,
			Status: "pending", CreatedAt: time.Now(),
		})
		if ierr != nil {
			return nil, ierr
		}
		return &domain.LLMDecision{ID: id, RequestID: req.RequestID, Action: action,
			Rationale: "review rationale", ConfidentScore: score, Status: "pending"}, nil
	}
}

func TestActionReviewAppliedToFreeTextReply(t *testing.T) {
	// A learned free-text reply goes through the consult review; the adapted
	// text is what reaches the pane and the audit, while learning still
	// records the ORIGINAL action.
	h := approvalReviewHarness(t, "")
	var calls atomic.Int32
	var reviewedCtx, reqID atomicString
	respondReview(h, &calls, 80, func(req domain.LLMRequest) (string, error) {
		reviewedCtx.set(req.ContextJSON)
		reqID.set(req.RequestID)
		return "y please, go ahead", nil
	})

	h.push("agent-ar1", "blocked")
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	if got := h.herdr.sentInputs()[0]; got != "y please, go ahead" {
		t.Errorf("sent %q, want the adapted text", got)
	}
	if calls.Load() != 1 {
		t.Errorf("want exactly one review consult, saw %d", calls.Load())
	}

	// The review context carries the proposed action and the answer contract.
	m := decodeContext(t, reviewedCtx.get())
	if pa, _ := m["proposed_action"].(string); pa != "y" {
		t.Errorf("proposed_action = %q, want the original reply", pa)
	}
	if af, _ := m["answer_format"].(string); !strings.Contains(af, domain.ActionSendProposedAction) {
		t.Errorf("answer_format should document the affirm sentinel: %q", af)
	}

	audits, err := h.raw.AuditLog(context.Background(), 10)
	if err != nil || len(audits) == 0 {
		t.Fatalf("audit log: %v %v", audits, err)
	}
	if audits[0].Input != "y please, go ahead" || audits[0].Status != "auto" {
		t.Errorf("audit should carry the delivered text: %+v", audits[0])
	}
	if audits[0].LLMOutput != "y please, go ahead" {
		t.Errorf("audit LLMOutput should carry the LLM's actual output: %q", audits[0].LLMOutput)
	}
	if !strings.Contains(audits[0].Rationale, "rewritten by llm.command (rewrite action)") ||
		!strings.Contains(audits[0].Rationale, `"y"`) {
		t.Errorf("audit rationale should note the review and the original: %q", audits[0].Rationale)
	}
	// The review's self-reported score is recorded for observability.
	if audits[0].LLMConfidence == nil || *audits[0].LLMConfidence != 80 {
		t.Errorf("audit LLMConfidence = %v, want 80", audits[0].LLMConfidence)
	}

	// Learning stays on the original — adapted text must not enter history.
	decs, err := h.raw.DecisionsForSignature(context.Background(), audits[0].Signature, 50)
	if err != nil || len(decs) == 0 {
		t.Fatalf("decisions: %v %v", decs, err)
	}
	if decs[0].ChosenAction != "y" {
		t.Errorf("learned %q, want the original action \"y\"", decs[0].ChosenAction)
	}
	// The delivered review's decision row resolves to accepted.
	waitFor(t, 3*time.Second, func() bool {
		d, err := h.raw.LLMDecisionByRequest(context.Background(), reqID.get())
		return err == nil && d != nil && d.Status == "accepted"
	})
}

func TestActionReviewSkippedForMenuMappedApproval(t *testing.T) {
	// A numbered-menu answer must reach the menu as the digit, untouched by
	// the review.
	h := newHarness(t, reviewCfg(""))
	h.herdr.setPane(approvalPane)
	h.seedAutonomous(approvalPane, domain.SituationApproval, "Yes")
	h.llm.configured = true
	var calls atomic.Int32
	respondReview(h, &calls, 90, func(domain.LLMRequest) (string, error) {
		return "SHOULD NEVER BE SENT", nil
	})

	h.push("agent-ar2", "blocked")
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	if got := h.herdr.sentInputs()[0]; got != "1" {
		t.Errorf("sent %q, want the menu digit \"1\"", got)
	}
	if calls.Load() != 0 {
		t.Errorf("the LLM must not be consulted for menu answers, saw %d calls", calls.Load())
	}
}

func TestActionReviewSkippedForDeclaredTask(t *testing.T) {
	// A declared task from a [[task_sources]] is never action-reviewed: the
	// source's enable_llm_review gate owns task review, and an opted-out
	// source delivers its tasks verbatim.
	dir := t.TempDir()
	taskFile := filepath.Join(dir, "tasks.md")
	os.WriteFile(taskFile, []byte("- [ ] step two\n"), 0o600)
	idlePane := "All tests pass. Task is complete.\n"
	cfg := reviewCfg("") +
		fmt.Sprintf("\n[[task_sources]]\nagent = \"agent-ar3\"\npath = %q\nenable_llm_review = false\n", taskFile)
	h := newHarness(t, cfg)
	h.herdr.setPane(idlePane)
	h.seedAutonomous(idlePane, domain.SituationIdle, domain.ActionNextDeclaredTask)
	h.llm.configured = true
	var calls atomic.Int32
	respondReview(h, &calls, 90, func(domain.LLMRequest) (string, error) {
		return "SHOULD NEVER BE SENT", nil
	})
	name, err := h.raw.EnsureAgentName(context.Background(), "agent-ar3")
	if err != nil {
		t.Fatal(err)
	}
	original := (&domain.DeclaredTask{Task: "step two", Path: taskFile, AgentName: name}).Prompt()

	h.push("agent-ar3", "idle")
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	if got := h.herdr.sentInputs()[0]; got != original {
		t.Errorf("sent %q, want the declared task verbatim %q", got, original)
	}
	if calls.Load() != 0 {
		t.Errorf("declared-task sends must never be reviewed, saw %d consults", calls.Load())
	}
}

func TestActionReviewDisabledSendsDirectly(t *testing.T) {
	// enable_rewrite_action defaults to false: with an LLM configured but the
	// flag off, the learned reply is sent directly and no consult fires.
	h := newHarness(t, "[llm]\ncommand = [\"fake\"]\ntimeout_seconds = 5\n")
	h.herdr.setPane(freeApprovalPane)
	h.seedAutonomous(freeApprovalPane, domain.SituationApproval, "y")
	h.llm.configured = true
	var calls atomic.Int32
	respondReview(h, &calls, 90, func(domain.LLMRequest) (string, error) {
		return "SHOULD NEVER BE SENT", nil
	})

	h.push("agent-ar4", "blocked")
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	if got := h.herdr.sentInputs()[0]; got != "y" {
		t.Errorf("sent %q, want the original \"y\"", got)
	}
	if calls.Load() != 0 {
		t.Errorf("review disabled must not consult, saw %d calls", calls.Load())
	}
}

func TestActionReviewFailureSendsOriginalAsIs(t *testing.T) {
	// A consult failure never blocks the send — the original is delivered
	// exactly as it was, unwrapped (the default fallback is passthrough).
	h := approvalReviewHarness(t, "")
	var calls atomic.Int32
	respondReview(h, &calls, 0, func(domain.LLMRequest) (string, error) {
		return "", errors.New("induced review failure")
	})

	h.push("agent-ar5", "blocked")
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	if got := h.herdr.sentInputs()[0]; got != "y" {
		t.Errorf("sent %q, want the original as-is", got)
	}

	audits, _ := h.raw.AuditLog(context.Background(), 10)
	if len(audits) == 0 || !strings.Contains(audits[0].Rationale, "action review failed") ||
		!strings.Contains(audits[0].Rationale, "fallback template applied") {
		t.Errorf("audit rationale should note the failed review: %+v", audits)
	}
	if len(audits) > 0 && !strings.Contains(audits[0].LLMOutput, "induced review failure") {
		t.Errorf("audit LLMOutput should carry review diagnostics: %q", audits[0].LLMOutput)
	}
}

func TestActionReviewCustomFallbackTemplate(t *testing.T) {
	h := approvalReviewHarness(t, "rewrite_action_fallback_template = \"Operator rule says: {original_text}\"\n")
	var calls atomic.Int32
	respondReview(h, &calls, 0, func(domain.LLMRequest) (string, error) {
		return "", errors.New("nope")
	})

	h.push("agent-ar6", "blocked")
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	want := "Operator rule says: y"
	if got := h.herdr.sentInputs()[0]; got != want {
		t.Errorf("sent %q, want custom-template fallback %q", got, want)
	}
}

func TestActionReviewLegacyFallbackTemplateKeyMigrates(t *testing.T) {
	// The renamed rewrite_fallback_template key still applies via the config
	// migration.
	h := approvalReviewHarness(t, "rewrite_fallback_template = \"Legacy says: {original_text}\"\n")
	var calls atomic.Int32
	respondReview(h, &calls, 0, func(domain.LLMRequest) (string, error) {
		return "", errors.New("nope")
	})

	h.push("agent-ar7", "blocked")
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	if got := h.herdr.sentInputs()[0]; got != "Legacy says: y" {
		t.Errorf("sent %q, want the migrated legacy template applied", got)
	}
}

func TestActionReviewEmptyOutputSendsOriginal(t *testing.T) {
	// An empty submitted action (no adapter error) degrades to the original.
	h := approvalReviewHarness(t, "")
	var calls atomic.Int32
	respondReview(h, &calls, 50, func(domain.LLMRequest) (string, error) {
		return "", nil
	})

	h.push("agent-ar8", "blocked")
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	if got := h.herdr.sentInputs()[0]; got != "y" {
		t.Errorf("sent %q, want the original as-is", got)
	}
	audits, _ := h.raw.AuditLog(context.Background(), 10)
	if len(audits) == 0 || !strings.Contains(audits[0].Rationale, "empty output") {
		t.Errorf("audit rationale should note the empty review: %+v", audits)
	}
}

func TestActionReviewNoDecisionSendsOriginal(t *testing.T) {
	// A consult that returns neither decision nor error (the CLI never
	// called submit_decision) degrades to the original.
	h := approvalReviewHarness(t, "")
	h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		return nil, nil
	}

	h.push("agent-ar9", "blocked")
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	if got := h.herdr.sentInputs()[0]; got != "y" {
		t.Errorf("sent %q, want the original as-is", got)
	}
	audits, _ := h.raw.AuditLog(context.Background(), 10)
	if len(audits) == 0 || !strings.Contains(audits[0].Rationale, "no decision") {
		t.Errorf("audit rationale should note the missing decision: %+v", audits)
	}
}

func TestActionReviewAffirmSentinelSendsOriginalVerbatim(t *testing.T) {
	// "@proposed_action:send" affirms the original: it is sent verbatim,
	// even when a custom fallback template is configured — the template
	// frames failures, not agreements. Case variants are tolerated (the
	// sentinel is matched EqualFold, like the removed @rewrite:nochange).
	h := approvalReviewHarness(t, "rewrite_action_fallback_template = \"Operator rule says: {original_text}\"\n")
	var calls atomic.Int32
	respondReview(h, &calls, 70, func(domain.LLMRequest) (string, error) {
		return "@Proposed_Action:Send", nil
	})

	h.push("agent-ar10", "blocked")
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	if got := h.herdr.sentInputs()[0]; got != "y" {
		t.Errorf("sent %q, want the original verbatim", got)
	}
	audits, _ := h.raw.AuditLog(context.Background(), 10)
	if len(audits) == 0 || !strings.Contains(audits[0].Rationale, domain.ActionSendProposedAction) {
		t.Errorf("audit rationale should note the affirm sentinel: %+v", audits)
	}
	// Learning stays on the original — the sentinel must not enter history.
	if len(audits) > 0 {
		decs, _ := h.raw.DecisionsForSignature(context.Background(), audits[0].Signature, 50)
		if len(decs) == 0 || decs[0].ChosenAction != "y" {
			t.Errorf("learned action drifted: %+v", decs)
		}
	}
}

func TestActionReviewTaskSentinelOutsideTaskReviewDegrades(t *testing.T) {
	// The task-review sentinel is meaningless on an action review: it must
	// never reach the pane as literal text — the original is sent instead.
	h := approvalReviewHarness(t, "")
	var calls atomic.Int32
	respondReview(h, &calls, 90, func(domain.LLMRequest) (string, error) {
		return domain.ActionSendProposed, nil
	})

	h.push("agent-ar11", "blocked")
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	if got := h.herdr.sentInputs()[0]; got != "y" {
		t.Errorf("sent %q, want the original (sentinel discarded)", got)
	}
	audits, _ := h.raw.AuditLog(context.Background(), 10)
	if len(audits) == 0 || !strings.Contains(audits[0].Rationale, "task-review sentinel") {
		t.Errorf("audit rationale should note the discarded sentinel: %+v", audits)
	}
}

func TestActionReviewLowConfidenceStillDelivers(t *testing.T) {
	// The consult confidence gate does NOT apply to action reviews: even a
	// score below the threshold still delivers the adapted text — the learned
	// rule already earned the send.
	h := approvalReviewHarness(t, "") // default auto_act_confidence_threshold (99)
	var calls atomic.Int32
	respondReview(h, &calls, 5, func(domain.LLMRequest) (string, error) {
		return "y — confirmed after review", nil
	})

	h.push("agent-ar12", "blocked")
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	if got := h.herdr.sentInputs()[0]; got != "y — confirmed after review" {
		t.Errorf("sent %q, want the adapted text despite the low score", got)
	}
	audits, _ := h.raw.AuditLog(context.Background(), 10)
	if len(audits) == 0 || audits[0].LLMConfidence == nil || *audits[0].LLMConfidence != 5 {
		t.Errorf("audit should record the low score for observability: %+v", audits)
	}
}

func TestActionReviewOutputTrippingNeverAutoFallsBack(t *testing.T) {
	// SC-5/FR-015: the reviewer is an LLM authoring outbound text — output
	// naming an irreversible operation is discarded and the safe original
	// is delivered as-is instead.
	h := approvalReviewHarness(t, "")
	var calls atomic.Int32
	var reqID atomicString
	respondReview(h, &calls, 90, func(req domain.LLMRequest) (string, error) {
		reqID.set(req.RequestID)
		return "sounds good, just force-push the branch afterwards", nil
	})

	h.push("agent-ar13", "blocked")
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	if got := h.herdr.sentInputs()[0]; got != "y" {
		t.Errorf("sent %q, want the dangerous review discarded for the original", got)
	}
	audits, _ := h.raw.AuditLog(context.Background(), 10)
	if len(audits) == 0 || !strings.Contains(audits[0].Rationale, "never-auto") {
		t.Errorf("audit should note the discarded review: %+v", audits)
	}
	// A degraded review's decision row resolves to rejected — its output was
	// discarded, not delivered.
	waitFor(t, 3*time.Second, func() bool {
		d, err := h.raw.LLMDecisionByRequest(context.Background(), reqID.get())
		return err == nil && d != nil && d.Status == "rejected"
	})
}

func TestActionReviewFallbackAlsoTrippingEscalates(t *testing.T) {
	// If even the fallback-wrapped original trips the safety screen (here
	// via a booby-trapped operator template), nothing is sent and the
	// situation escalates with the original as the confirmable suggestion.
	h := approvalReviewHarness(t, "rewrite_action_fallback_template = \"force-push first, then: {original_text}\"\n")
	var calls atomic.Int32
	respondReview(h, &calls, 0, func(domain.LLMRequest) (string, error) {
		return "", errors.New("induced failure")
	})

	h.push("agent-ar14", "blocked")
	waitFor(t, 3*time.Second, func() bool {
		audits, _ := h.raw.AuditLog(context.Background(), 10)
		for _, a := range audits {
			if a.Status == "escalated" {
				return true
			}
		}
		return false
	})
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("nothing must be sent when the fallback trips the allowlist, sent %v", got)
	}
	audits, _ := h.raw.AuditLog(context.Background(), 10)
	if !strings.Contains(audits[0].Suggestion, "respond: ") {
		t.Errorf("escalation should carry a confirmable original suggestion: %+v", audits[0])
	}
}

func TestActionReviewStaleSituationDropsSend(t *testing.T) {
	// The pane moved on while the review ran: nothing is sent, nothing
	// escalates — the new situation drives its own pipeline event.
	release := make(chan struct{})
	var calls atomic.Int32
	h := approvalReviewHarness(t, "")
	respondReview(h, &calls, 90, func(domain.LLMRequest) (string, error) {
		<-release
		return "y please", nil
	})

	h.push("agent-ar15", "blocked")
	waitFor(t, 3*time.Second, func() bool { return calls.Load() == 1 })
	h.herdr.setPane("compiling project, please wait...\n") // situation gone
	close(release)

	time.Sleep(300 * time.Millisecond)
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("stale review must not send, sent %v", got)
	}
	audits, _ := h.raw.AuditLog(context.Background(), 10)
	for _, a := range audits {
		if a.Status == "auto" || a.Status == "escalated" {
			t.Errorf("stale review must neither act nor escalate: %+v", a)
		}
	}
}

func TestActionReviewDuplicateTransitionSendsOnce(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	h := approvalReviewHarness(t, "")
	respondReview(h, &calls, 90, func(domain.LLMRequest) (string, error) {
		<-release
		return "reviewed once", nil
	})

	h.push("agent-ar16", "blocked")
	waitFor(t, 3*time.Second, func() bool { return calls.Load() == 1 })
	// The same situation fires again while the first review is in flight.
	h.push("agent-ar16", "blocked")
	time.Sleep(200 * time.Millisecond)
	close(release)

	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	time.Sleep(300 * time.Millisecond)
	if got := h.herdr.sentInputs(); len(got) != 1 {
		t.Errorf("duplicate in-flight review must send exactly once, sent %v", got)
	}
	if calls.Load() != 1 {
		t.Errorf("duplicate transition must not spawn a second review, saw %d", calls.Load())
	}
}

func TestActionReviewKillSwitchMidFlightEscalates(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	h := approvalReviewHarness(t, "")
	respondReview(h, &calls, 90, func(domain.LLMRequest) (string, error) {
		<-release
		return "too late", nil
	})

	h.push("agent-ar17", "blocked")
	waitFor(t, 3*time.Second, func() bool { return calls.Load() == 1 })
	h.raw.InsertKillEvent(context.Background(), domain.KillEvent{
		State: "active", Scope: "global", Author: "test", CreatedAt: time.Now(),
	})
	close(release)

	waitFor(t, 3*time.Second, func() bool {
		audits, _ := h.raw.AuditLog(context.Background(), 10)
		for _, a := range audits {
			if a.Status == "escalated" && strings.Contains(a.Rationale, "[daemon_paused]") {
				return true
			}
		}
		return false
	})
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("kill switch must block the reviewed send, sent %v", got)
	}
}

func TestActionReviewSupersededByNewSituationDropsOldFlight(t *testing.T) {
	// A new decision for the agent (here: a menu approval) cancels the
	// in-flight free-text review; the old outcome must be dropped by the
	// token check, its context cancelled so the CLI stops burning tokens,
	// and its staged request expired so it cannot block later consults.
	release := make(chan struct{})
	cancelled := make(chan struct{}, 1)
	var calls atomic.Int32
	h := approvalReviewHarness(t, "")
	h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		calls.Add(1)
		select {
		case <-ctx.Done():
			cancelled <- struct{}{}
			return nil, ctx.Err()
		case <-release:
			return &domain.LLMDecision{Action: "stale review"}, nil
		}
	}

	h.push("agent-ar18", "blocked")
	waitFor(t, 3*time.Second, func() bool { return calls.Load() == 1 })

	// The pane now shows a numbered approval: a different situation whose
	// decision (digit send) must invalidate the free-text flight.
	h.herdr.setPane(approvalPane)
	h.seedAutonomous(approvalPane, domain.SituationApproval, "Yes")
	h.push("agent-ar18", "blocked")

	waitFor(t, 3*time.Second, func() bool { // the flight's context is cancelled
		select {
		case <-cancelled:
			return true
		default:
			return false
		}
	})
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	close(release)
	time.Sleep(300 * time.Millisecond)
	sent := h.herdr.sentInputs()
	if len(sent) != 1 || sent[0] != "1" {
		t.Errorf("only the new decision's digit may send, got %v", sent)
	}
	// The superseded flight's staged request must not linger as pending.
	pending, err := h.raw.HasPendingLLMConsult(context.Background(), "agent-ar18")
	if err != nil || pending {
		t.Errorf("superseded review request should be expired, pending=%v err=%v", pending, err)
	}
}

func TestActionReviewRateGuardAtDeliveryEscalates(t *testing.T) {
	// The send lands up to the consult timeout after Decide: a runaway
	// counter that filled up in between must still block it.
	release := make(chan struct{})
	var calls atomic.Int32
	h := approvalReviewHarness(t, "")
	respondReview(h, &calls, 90, func(domain.LLMRequest) (string, error) {
		<-release
		return "reviewed", nil
	})

	h.push("agent-ar19", "blocked")
	waitFor(t, 3*time.Second, func() bool { return calls.Load() == 1 })
	h.raw.UpdateAgentRate(context.Background(), domain.AgentRate{
		AgentID: "agent-ar19", ConsecutiveAuto: 1000, WindowStart: time.Now(), CountInWindow: 1000,
	})
	close(release)

	waitFor(t, 3*time.Second, func() bool {
		audits, _ := h.raw.AuditLog(context.Background(), 10)
		for _, a := range audits {
			if a.Status == "escalated" && strings.Contains(a.Rationale, "[rate_limited]") {
				return true
			}
		}
		return false
	})
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("rate guard must block the reviewed send, sent %v", got)
	}
}

func TestActionReviewSignatureChangeSameTypeDropsSend(t *testing.T) {
	// Approval → different approval while the review ran: same type, new
	// signature — the learned answer belongs to the OLD dialog, so nothing
	// may send.
	release := make(chan struct{})
	var calls atomic.Int32
	h := approvalReviewHarness(t, "")
	respondReview(h, &calls, 90, func(domain.LLMRequest) (string, error) {
		<-release
		return "y please", nil
	})

	h.push("agent-ar20", "blocked")
	waitFor(t, 3*time.Second, func() bool { return calls.Load() == 1 })
	// Still an approval, but a different permission dialog.
	h.herdr.setPane("Bash(rm campsite.txt)\nDo you want to proceed? (y/n)\n")
	close(release)

	time.Sleep(300 * time.Millisecond)
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("signature drift must drop the send, sent %v", got)
	}
}

func TestActionReviewIdleInferredContentDriftStillDelivers(t *testing.T) {
	// Policy pin: idle staleness matches on TYPE only. An inferred-task idle
	// send is reviewed (only DECLARED tasks are exempt), and a still-idle
	// pane with different text keeps the send.
	idleTodoPane := "  ⎿  ✔ parse input\n     □ validate fields\n     □ emit output\n"
	release := make(chan struct{})
	var calls atomic.Int32
	h := newHarness(t, reviewCfg(""))
	h.herdr.setPane(idleTodoPane)
	h.seedAutonomous(idleTodoPane, domain.SituationIdle, domain.ActionNextInferredTask)
	h.llm.configured = true
	respondReview(h, &calls, 90, func(domain.LLMRequest) (string, error) {
		<-release
		return "reviewed idle prompt", nil
	})

	h.push("agent-ar21", "idle")
	waitFor(t, 3*time.Second, func() bool { return calls.Load() == 1 })
	h.herdr.setPane("Finished formatting. Everything is done here.\n") // still idle, new words
	close(release)

	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	if got := h.herdr.sentInputs()[0]; got != "reviewed idle prompt" {
		t.Errorf("sent %q, want the reviewed prompt despite idle content drift", got)
	}
	// And the symbolic inferred action is what was learned.
	audits, _ := h.raw.AuditLog(context.Background(), 10)
	if len(audits) > 0 {
		decs, _ := h.raw.DecisionsForSignature(context.Background(), audits[0].Signature, 50)
		if len(decs) == 0 || decs[0].ChosenAction != domain.ActionNextInferredTask {
			t.Errorf("learned %v, want the symbolic inferred action", decs)
		}
	}
}

func TestActionReviewAuditFailureBlocksSend(t *testing.T) {
	// FR-024 holds on the review path too: no audit record, no send — and
	// the decision must not be recorded as accepted for an undelivered
	// action.
	release := make(chan struct{})
	var calls atomic.Int32
	var reqID atomicString
	h := approvalReviewHarness(t, "")
	respondReview(h, &calls, 90, func(req domain.LLMRequest) (string, error) {
		reqID.set(req.RequestID)
		<-release
		return "reviewed", nil
	})

	h.push("agent-ar22", "blocked")
	waitFor(t, 3*time.Second, func() bool { return calls.Load() == 1 })
	h.store.(*failingStore).setFailAudit(true)
	close(release)

	waitFor(t, 3*time.Second, func() bool {
		for _, n := range h.herdr.notified() {
			if strings.Contains(n, "persistence failure") {
				return true
			}
		}
		return false
	})
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("audit failure must block the send (FR-024), sent %v", got)
	}
	waitFor(t, 3*time.Second, func() bool {
		d, err := h.raw.LLMDecisionByRequest(context.Background(), reqID.get())
		return err == nil && d != nil && d.Status == "rejected"
	})
}

func TestActionReviewNoopSendsNothing(t *testing.T) {
	// "@noop": the LLM vetoed the send. Nothing reaches the pane, a noop
	// audit row lands, the runaway counter advances — and the learned rule
	// is untouched (no @noop decision recorded, so the veto cannot stand
	// the learned rule down permanently).
	h := approvalReviewHarness(t, "")
	var calls atomic.Int32
	respondReview(h, &calls, 90, func(domain.LLMRequest) (string, error) {
		return "@noop", nil
	})

	h.push("agent-ar23", "blocked")
	// The rate write is the LAST persistence step in deliverActionReviewNoop;
	// waiting on it (not the audit, which lands first) avoids racing the
	// delivery tail.
	waitFor(t, 3*time.Second, func() bool {
		rate, err := h.raw.GetAgentRate(context.Background(), "agent-ar23")
		return err == nil && rate.ConsecutiveAuto == 1
	})
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("@noop must not send, sent %v", got)
	}

	audits, _ := h.raw.AuditLog(context.Background(), 10)
	if len(audits) == 0 || audits[0].Status != "auto" || audits[0].Action != "noop" {
		t.Fatalf("want a noop auto audit row first, got %+v", audits)
	}
	noop := audits[0]
	if noop.Input != "" || noop.LLMOutput != domain.ActionNoop {
		t.Errorf("noop audit row malformed: %+v", noop)
	}
	if !strings.Contains(noop.Rationale, "llm review declined to send") ||
		!strings.Contains(noop.Rationale, `"y"`) {
		t.Errorf("noop rationale should carry the veto and the original: %q", noop.Rationale)
	}

	decs, err := h.raw.DecisionsForSignature(context.Background(), noop.Signature, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, dec := range decs {
		if dec.ChosenAction == domain.ActionNoop {
			t.Errorf("review noop must not be recorded as a decision: %+v", dec)
		}
	}
}

func TestActionReviewNoopRateGuardEscalates(t *testing.T) {
	// A saturated runaway counter beats even a noop outcome: the situation
	// escalates (suggesting "do nothing") instead of silently nooping —
	// the operator must see a rate-limited agent, whatever the LLM said.
	release := make(chan struct{})
	var calls atomic.Int32
	h := approvalReviewHarness(t, "")
	respondReview(h, &calls, 90, func(domain.LLMRequest) (string, error) {
		<-release
		return "@noop", nil
	})

	h.push("agent-ar24", "blocked")
	waitFor(t, 3*time.Second, func() bool { return calls.Load() == 1 })
	h.raw.UpdateAgentRate(context.Background(), domain.AgentRate{
		AgentID: "agent-ar24", ConsecutiveAuto: 1000, WindowStart: time.Now(), CountInWindow: 1000,
	})
	close(release)

	waitFor(t, 3*time.Second, func() bool {
		audits, _ := h.raw.AuditLog(context.Background(), 10)
		for _, a := range audits {
			if a.Status == "escalated" && strings.Contains(a.Rationale, "[rate_limited]") {
				return true
			}
		}
		return false
	})
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("rate guard must block everything, sent %v", got)
	}
	audits, _ := h.raw.AuditLog(context.Background(), 10)
	for _, a := range audits {
		if a.Status == "escalated" && a.Suggestion != domain.ActionNoopSuggestion {
			t.Errorf("escalation should suggest doing nothing, got %q", a.Suggestion)
		}
		if a.Status == "auto" {
			t.Errorf("a rate-limited noop must not audit as auto: %+v", a)
		}
	}
}

func TestActionReviewNoopSpellingNormalized(t *testing.T) {
	// The consult path normalizes bare noop spellings at submission
	// (mcpserver) and defensively in the handler, so a review answering
	// "no_op" stands down like "@noop". This is a deliberate change from
	// the removed rewrite CLI, whose raw stdout only honored the @ prefix.
	h := approvalReviewHarness(t, "")
	var calls atomic.Int32
	respondReview(h, &calls, 90, func(domain.LLMRequest) (string, error) {
		return "no_op", nil
	})
	h.push("agent-ar25", "blocked")
	waitFor(t, 3*time.Second, func() bool {
		audits, _ := h.raw.AuditLog(context.Background(), 10)
		return len(audits) > 0 && audits[0].Action == "noop"
	})
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("normalized noop must stand down, sent %v", got)
	}
}

func TestActionReviewNoopKillSwitchEscalates(t *testing.T) {
	// A kill switch raised mid-flight still wins over a noop outcome, and
	// the escalation suggests doing nothing — not sending the original the
	// LLM just vetoed.
	release := make(chan struct{})
	var calls atomic.Int32
	h := approvalReviewHarness(t, "")
	respondReview(h, &calls, 90, func(domain.LLMRequest) (string, error) {
		<-release
		return "@noop", nil
	})

	h.push("agent-ar26", "blocked")
	waitFor(t, 3*time.Second, func() bool { return calls.Load() == 1 })
	h.raw.InsertKillEvent(context.Background(), domain.KillEvent{
		State: "active", Scope: "global", Author: "test", CreatedAt: time.Now(),
	})
	close(release)

	waitFor(t, 3*time.Second, func() bool {
		audits, _ := h.raw.AuditLog(context.Background(), 10)
		for _, a := range audits {
			if a.Status == "escalated" && strings.Contains(a.Rationale, "[daemon_paused]") {
				return true
			}
		}
		return false
	})
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("kill switch must block everything, sent %v", got)
	}
	audits, _ := h.raw.AuditLog(context.Background(), 10)
	for _, a := range audits {
		if a.Status == "escalated" && a.Suggestion != domain.ActionNoopSuggestion {
			t.Errorf("escalation should suggest doing nothing, got %q", a.Suggestion)
		}
		if a.Status == "auto" {
			t.Errorf("nothing may auto-run under the kill switch: %+v", a)
		}
	}
}

func TestActionReviewNoopDroppedWhenAgentResumes(t *testing.T) {
	// The agent resumes working while the review is in flight: the flight
	// is cancelled on the transition, so a late "@noop" outcome — which
	// deliberately skips the staleness re-read — must apply NO side
	// effects: no noop audit row, no runaway-counter advance.
	cancelled := make(chan struct{}, 1)
	release := make(chan struct{})
	var calls atomic.Int32
	h := approvalReviewHarness(t, "")
	h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		calls.Add(1)
		select {
		case <-ctx.Done():
			cancelled <- struct{}{}
			return nil, ctx.Err()
		case <-release:
			return &domain.LLMDecision{Action: "@noop"}, nil
		}
	}

	h.push("agent-ar27", "blocked")
	waitFor(t, 3*time.Second, func() bool { return calls.Load() == 1 })
	h.push("agent-ar27", "working") // resume supersedes the flight

	waitFor(t, 3*time.Second, func() bool {
		select {
		case <-cancelled:
			return true
		default:
			return false
		}
	})
	close(release)
	time.Sleep(300 * time.Millisecond)

	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("a superseded flight must not send, sent %v", got)
	}
	audits, _ := h.raw.AuditLog(context.Background(), 10)
	for _, a := range audits {
		if a.Action == "noop" {
			t.Errorf("stale noop must leave no audit row: %+v", a)
		}
	}
	rate, err := h.raw.GetAgentRate(context.Background(), "agent-ar27")
	if err != nil || rate.ConsecutiveAuto != 0 {
		t.Errorf("stale noop must not advance the runaway counter: %+v (%v)", rate, err)
	}
}

func TestActionReviewNoopAuditFailureNotifies(t *testing.T) {
	// FR-024 holds for the noop path too: no audit record, no state
	// advance — the operator is notified and the decision resolves as
	// rejected (the stand-down never applied).
	release := make(chan struct{})
	var calls atomic.Int32
	var reqID atomicString
	h := approvalReviewHarness(t, "")
	respondReview(h, &calls, 90, func(req domain.LLMRequest) (string, error) {
		reqID.set(req.RequestID)
		<-release
		return "@noop", nil
	})

	h.push("agent-ar28", "blocked")
	waitFor(t, 3*time.Second, func() bool { return calls.Load() == 1 })
	h.store.(*failingStore).setFailAudit(true)
	close(release)

	waitFor(t, 3*time.Second, func() bool {
		for _, n := range h.herdr.notified() {
			if strings.Contains(n, "persistence failure") {
				return true
			}
		}
		return false
	})
	rate, err := h.raw.GetAgentRate(context.Background(), "agent-ar28")
	if err != nil || rate.ConsecutiveAuto != 0 {
		t.Errorf("blocked noop must not advance the runaway counter: %+v (%v)", rate, err)
	}
	waitFor(t, 3*time.Second, func() bool {
		d, derr := h.raw.LLMDecisionByRequest(context.Background(), reqID.get())
		return derr == nil && d != nil && d.Status == "rejected"
	})
}

func TestActionReviewSentinelOnOrdinaryConsultEscalates(t *testing.T) {
	// The affirm sentinel is meaningless on an ordinary consult (no proposed
	// action to expand it to): the submission is rejected and escalated, and
	// the raw sentinel never becomes a confirmable suggestion.
	cfg := "[llm]\ncommand = [\"fake\"]\nauto_act_confidence_threshold = 0\ntimeout_seconds = 5\n"
	h := newHarness(t, cfg)
	h.herdr.setPane(freeApprovalPane)
	h.llm.configured = true
	h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		id, err := h.raw.InsertLLMDecision(ctx, domain.LLMDecision{
			RequestID: req.RequestID, Signature: req.Signature,
			SituationType: req.SituationType, AgentType: req.AgentType,
			Action: domain.ActionSendProposedAction, ConfidentScore: 95,
			Status: "pending", CreatedAt: time.Now(),
		})
		if err != nil {
			return nil, err
		}
		return &domain.LLMDecision{ID: id, RequestID: req.RequestID,
			Action: domain.ActionSendProposedAction, ConfidentScore: 95}, nil
	}

	h.push("agent-ar29", "blocked")
	waitFor(t, 5*time.Second, func() bool {
		audits, _ := h.raw.AuditLog(context.Background(), 10)
		for _, a := range audits {
			if a.Status == "escalated" {
				return true
			}
		}
		return false
	})
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("the sentinel must never be typed into the pane, sent %v", got)
	}
	audits, _ := h.raw.AuditLog(context.Background(), 10)
	for _, a := range audits {
		if a.Status == "escalated" && strings.Contains(a.Suggestion, domain.ActionSendProposedAction) {
			t.Errorf("the raw sentinel must not be confirmable: %+v", a)
		}
	}
}
