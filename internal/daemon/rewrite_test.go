package daemon

// Tests for the optional outbound-text rewrite (llm.rewrite_command): the
// async pipeline in startRewrite/handleRewriteOutcome, its safety re-gates,
// and the never-block-the-send fallback semantics.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// idleRewriteHarness seeds a declared-task idle signature for the given
// agent and returns the harness, the rewriter fake, and the original prompt
// the pipeline would send without a rewriter.
func idleRewriteHarness(t *testing.T, agent, extraCfg string,
	rewrite func(ctx context.Context, req domain.RewriteRequest) (string, error)) (*harness, *fakeRewriter, string) {
	t.Helper()
	dir := t.TempDir()
	taskFile := filepath.Join(dir, "tasks.md")
	os.WriteFile(taskFile, []byte("- [ ] step two\n"), 0o600)

	idlePane := "All tests pass. Task is complete.\n"
	cfg := fmt.Sprintf("[[task_sources]]\nagent = %q\npath = %q\n%s", agent, taskFile, extraCfg)
	h, fr := newHarnessRewriter(t, cfg, rewrite)
	h.herdr.setPane(idlePane)
	h.seedAutonomous(idlePane, domain.SituationIdle, domain.ActionNextDeclaredTask)
	name, err := h.raw.EnsureAgentName(context.Background(), agent)
	if err != nil {
		t.Fatal(err)
	}
	original := (&domain.DeclaredTask{Task: "step two", Path: taskFile, AgentName: name}).Prompt()
	return h, fr, original
}

func TestRewriteAppliedToIdleDeclaredTask(t *testing.T) {
	// A literal idle prompt goes through the rewrite CLI; the rewritten
	// text is what reaches the pane and the audit, while learning still
	// records the symbolic action.
	h, fr, original := idleRewriteHarness(t, "agent-rw1", "",
		func(ctx context.Context, req domain.RewriteRequest) (string, error) {
			return "REWRITTEN: please do step two", nil
		})

	h.push("agent-rw1", "idle")
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	if got := h.herdr.sentInputs()[0]; got != "REWRITTEN: please do step two" {
		t.Errorf("sent %q, want the rewritten text", got)
	}

	calls := fr.rewriteCalls()
	if len(calls) != 1 || calls[0].Text != original {
		t.Fatalf("rewriter saw %+v, want one call with the original prompt %q", calls, original)
	}
	if calls[0].SituationType != domain.SituationIdle || calls[0].PaneExcerpt == "" {
		t.Errorf("rewrite request missing context: %+v", calls[0])
	}
	// The agent's short name rides on the request for {agent_name}.
	wantName, _ := h.raw.EnsureAgentName(context.Background(), "agent-rw1")
	if calls[0].AgentName == "" || calls[0].AgentName != wantName {
		t.Errorf("rewrite request agent name = %q, want resolved short name %q", calls[0].AgentName, wantName)
	}

	audits, err := h.raw.AuditLog(context.Background(), 10)
	if err != nil || len(audits) == 0 {
		t.Fatalf("audit log: %v %v", audits, err)
	}
	if audits[0].Input != "REWRITTEN: please do step two" || audits[0].Status != "auto" {
		t.Errorf("audit should carry the delivered text: %+v", audits[0])
	}
	if !strings.Contains(audits[0].Rationale, "rewritten by llm.rewrite_command") ||
		!strings.Contains(audits[0].Rationale, "step two") {
		t.Errorf("audit rationale should note the rewrite and the original: %q", audits[0].Rationale)
	}

	// Learning stays symbolic — rewritten text must not enter history.
	decs, err := h.raw.DecisionsForSignature(context.Background(), audits[0].Signature, 50)
	if err != nil || len(decs) == 0 {
		t.Fatalf("decisions: %v %v", decs, err)
	}
	if decs[0].ChosenAction != domain.ActionNextDeclaredTask {
		t.Errorf("learned %q, want %q", decs[0].ChosenAction, domain.ActionNextDeclaredTask)
	}
}

func TestRewriteSkippedForMenuMappedApproval(t *testing.T) {
	// A numbered-menu answer must reach the menu as the digit, untouched by
	// the rewriter.
	h, fr := newHarnessRewriter(t, "",
		func(ctx context.Context, req domain.RewriteRequest) (string, error) {
			return "SHOULD NEVER BE SENT", nil
		})
	h.herdr.setPane(approvalPane)
	h.seedAutonomous(approvalPane, domain.SituationApproval, "Yes")

	h.push("agent-rw2", "blocked")
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	if got := h.herdr.sentInputs()[0]; got != "1" {
		t.Errorf("sent %q, want the menu digit \"1\"", got)
	}
	if calls := fr.rewriteCalls(); len(calls) != 0 {
		t.Errorf("rewriter must not be consulted for menu answers, saw %d calls", len(calls))
	}
}

func TestRewriteFailureSendsFallbackWrappedOriginal(t *testing.T) {
	// A rewrite failure never blocks the send — the original is delivered
	// inside the default quoting template.
	h, _, original := idleRewriteHarness(t, "agent-rw3", "",
		func(ctx context.Context, req domain.RewriteRequest) (string, error) {
			return "", errors.New("induced rewrite failure")
		})

	h.push("agent-rw3", "idle")
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	want := "You must act based on the following: " + original
	if got := h.herdr.sentInputs()[0]; got != want {
		t.Errorf("sent %q, want fallback-wrapped original %q", got, want)
	}

	audits, _ := h.raw.AuditLog(context.Background(), 10)
	if len(audits) == 0 || !strings.Contains(audits[0].Rationale, "rewrite failed") ||
		!strings.Contains(audits[0].Rationale, "fallback template applied") {
		t.Errorf("audit rationale should note the failed rewrite: %+v", audits)
	}
	if len(audits) > 0 && !strings.Contains(audits[0].LLMOutput, "induced rewrite failure") {
		t.Errorf("audit LLMOutput should carry rewrite diagnostics: %q", audits[0].LLMOutput)
	}
}

func TestRewriteCustomFallbackTemplate(t *testing.T) {
	extra := "[llm]\nrewrite_fallback_template = \"Operator rule says: {original_text}\"\n"
	h, _, original := idleRewriteHarness(t, "agent-rw4", extra,
		func(ctx context.Context, req domain.RewriteRequest) (string, error) {
			return "", errors.New("nope")
		})

	h.push("agent-rw4", "idle")
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	want := "Operator rule says: " + original
	if got := h.herdr.sentInputs()[0]; got != want {
		t.Errorf("sent %q, want custom-template fallback %q", got, want)
	}
}

func TestRewrittenTextTrippingNeverAutoFallsBack(t *testing.T) {
	// SC-5/FR-015: the rewriter is an LLM authoring outbound text — output
	// naming an irreversible operation is discarded and the safe original
	// (wrapped) is delivered instead.
	h, _, original := idleRewriteHarness(t, "agent-rw5", "",
		func(ctx context.Context, req domain.RewriteRequest) (string, error) {
			return "sounds good, just force-push the branch afterwards", nil
		})

	h.push("agent-rw5", "idle")
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	want := "You must act based on the following: " + original
	if got := h.herdr.sentInputs()[0]; got != want {
		t.Errorf("sent %q, want the dangerous rewrite discarded for %q", got, want)
	}
	audits, _ := h.raw.AuditLog(context.Background(), 10)
	if len(audits) == 0 || !strings.Contains(audits[0].Rationale, "never-auto") {
		t.Errorf("audit should note the discarded rewrite: %+v", audits)
	}
}

func TestRewriteFallbackAlsoTrippingEscalates(t *testing.T) {
	// If even the fallback-wrapped original trips the safety screen (here
	// via a booby-trapped operator template), nothing is sent and the
	// situation escalates with the original as the confirmable suggestion.
	extra := "[llm]\nrewrite_fallback_template = \"force-push first, then: {original_text}\"\n"
	h, _, _ := idleRewriteHarness(t, "agent-rw6", extra,
		func(ctx context.Context, req domain.RewriteRequest) (string, error) {
			return "", errors.New("induced failure")
		})

	h.push("agent-rw6", "idle")
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
	if !strings.Contains(audits[0].Suggestion, "send next declared task: ") {
		t.Errorf("escalation should carry a confirmable original suggestion: %+v", audits[0])
	}
}

func TestRewriteStaleSituationDropsSend(t *testing.T) {
	// The pane moved on while the rewrite ran: nothing is sent, nothing
	// escalates — the new situation drives its own pipeline event.
	freeApproval := "Do you want to continue? (y/n)\n"
	release := make(chan struct{})
	h, fr := newHarnessRewriter(t, "",
		func(ctx context.Context, req domain.RewriteRequest) (string, error) {
			<-release
			return "y please", nil
		})
	h.herdr.setPane(freeApproval)
	h.seedAutonomous(freeApproval, domain.SituationApproval, "y")

	h.push("agent-rw7", "blocked")
	waitFor(t, 3*time.Second, func() bool { return len(fr.rewriteCalls()) == 1 })
	h.herdr.setPane("compiling project, please wait...\n") // situation gone
	close(release)

	time.Sleep(300 * time.Millisecond)
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("stale rewrite must not send, sent %v", got)
	}
	audits, _ := h.raw.AuditLog(context.Background(), 10)
	for _, a := range audits {
		if a.Status == "auto" || a.Status == "escalated" {
			t.Errorf("stale rewrite must neither act nor escalate: %+v", a)
		}
	}
}

func TestRewriteDuplicateTransitionSendsOnce(t *testing.T) {
	release := make(chan struct{})
	h, fr, _ := idleRewriteHarness(t, "agent-rw8", "",
		func(ctx context.Context, req domain.RewriteRequest) (string, error) {
			<-release
			return "rewritten once", nil
		})

	h.push("agent-rw8", "idle")
	waitFor(t, 3*time.Second, func() bool { return len(fr.rewriteCalls()) == 1 })
	// The same situation fires again while the first rewrite is in flight.
	h.push("agent-rw8", "idle")
	time.Sleep(200 * time.Millisecond)
	close(release)

	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	time.Sleep(300 * time.Millisecond)
	if got := h.herdr.sentInputs(); len(got) != 1 {
		t.Errorf("duplicate in-flight rewrite must send exactly once, sent %v", got)
	}
	if calls := fr.rewriteCalls(); len(calls) != 1 {
		t.Errorf("duplicate transition must not spawn a second rewrite, saw %d", len(calls))
	}
}

func TestRewriteKillSwitchMidFlightEscalates(t *testing.T) {
	release := make(chan struct{})
	h, fr, _ := idleRewriteHarness(t, "agent-rw9", "",
		func(ctx context.Context, req domain.RewriteRequest) (string, error) {
			<-release
			return "too late", nil
		})

	h.push("agent-rw9", "idle")
	waitFor(t, 3*time.Second, func() bool { return len(fr.rewriteCalls()) == 1 })
	h.raw.InsertKillEvent(context.Background(), domain.KillEvent{
		State: "active", Scope: "global", Author: "test", CreatedAt: time.Now(),
	})
	close(release)

	waitFor(t, 3*time.Second, func() bool {
		audits, _ := h.raw.AuditLog(context.Background(), 10)
		for _, a := range audits {
			if a.Status == "escalated" && strings.Contains(a.Rationale, "[killed]") {
				return true
			}
		}
		return false
	})
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("kill switch must block the rewritten send, sent %v", got)
	}
}

func TestRewriteSupersededByNewSituationDropsOldFlight(t *testing.T) {
	// A new decision for the agent (here: a menu approval) cancels the
	// in-flight idle rewrite; the old outcome must be dropped by the token
	// check and its context cancelled so the CLI stops burning tokens.
	release := make(chan struct{})
	cancelled := make(chan struct{}, 1)
	h, fr, _ := idleRewriteHarness(t, "agent-rw11", "",
		func(ctx context.Context, req domain.RewriteRequest) (string, error) {
			select {
			case <-ctx.Done():
				cancelled <- struct{}{}
				return "", ctx.Err()
			case <-release:
				return "stale rewrite", nil
			}
		})

	h.push("agent-rw11", "idle")
	waitFor(t, 3*time.Second, func() bool { return len(fr.rewriteCalls()) == 1 })

	// The pane now shows a numbered approval: a different situation whose
	// decision (digit send) must invalidate the idle flight.
	h.herdr.setPane(approvalPane)
	h.seedAutonomous(approvalPane, domain.SituationApproval, "Yes")
	h.push("agent-rw11", "blocked")

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
}

func TestRewriteRateGuardAtDeliveryEscalates(t *testing.T) {
	// The send lands up to the rewrite timeout after Decide: a runaway
	// counter that filled up in between must still block it.
	release := make(chan struct{})
	h, fr, _ := idleRewriteHarness(t, "agent-rw12", "",
		func(ctx context.Context, req domain.RewriteRequest) (string, error) {
			<-release
			return "rewritten", nil
		})

	h.push("agent-rw12", "idle")
	waitFor(t, 3*time.Second, func() bool { return len(fr.rewriteCalls()) == 1 })
	h.raw.UpdateAgentRate(context.Background(), domain.AgentRate{
		AgentID: "agent-rw12", ConsecutiveAuto: 1000, WindowStart: time.Now(), CountInWindow: 1000,
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
		t.Errorf("rate guard must block the rewritten send, sent %v", got)
	}
}

func TestRewriteSignatureChangeSameTypeDropsSend(t *testing.T) {
	// Approval → different approval while the rewrite ran: same type, new
	// signature — the learned answer belongs to the OLD dialog, so nothing
	// may send.
	freeApproval := "Do you want to continue? (y/n)\n"
	release := make(chan struct{})
	h, fr := newHarnessRewriter(t, "",
		func(ctx context.Context, req domain.RewriteRequest) (string, error) {
			<-release
			return "y please", nil
		})
	h.herdr.setPane(freeApproval)
	h.seedAutonomous(freeApproval, domain.SituationApproval, "y")

	h.push("agent-rw13", "blocked")
	waitFor(t, 3*time.Second, func() bool { return len(fr.rewriteCalls()) == 1 })
	// Still an approval, but a different permission dialog.
	h.herdr.setPane("Bash(rm campsite.txt)\nDo you want to proceed? (y/n)\n")
	close(release)

	time.Sleep(300 * time.Millisecond)
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("signature drift must drop the send, sent %v", got)
	}
}

func TestRewriteIdleContentDriftStillDelivers(t *testing.T) {
	// Policy pin: idle staleness matches on TYPE only. Idle content
	// legitimately differs between the recent-delta classification read and
	// the visible re-read, so a still-idle pane with different text keeps
	// the send.
	release := make(chan struct{})
	h, fr, _ := idleRewriteHarness(t, "agent-rw14", "",
		func(ctx context.Context, req domain.RewriteRequest) (string, error) {
			<-release
			return "rewritten idle prompt", nil
		})

	h.push("agent-rw14", "idle")
	waitFor(t, 3*time.Second, func() bool { return len(fr.rewriteCalls()) == 1 })
	h.herdr.setPane("Finished formatting. Everything is done here.\n") // still idle, new words
	close(release)

	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	if got := h.herdr.sentInputs()[0]; got != "rewritten idle prompt" {
		t.Errorf("sent %q, want the rewritten prompt despite idle content drift", got)
	}
}

func TestRewriteAuditFailureBlocksSend(t *testing.T) {
	// FR-024 holds on the rewrite path too: no audit record, no send.
	release := make(chan struct{})
	h, fr, _ := idleRewriteHarness(t, "agent-rw10", "",
		func(ctx context.Context, req domain.RewriteRequest) (string, error) {
			<-release
			return "rewritten", nil
		})

	h.push("agent-rw10", "idle")
	waitFor(t, 3*time.Second, func() bool { return len(fr.rewriteCalls()) == 1 })
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
}
