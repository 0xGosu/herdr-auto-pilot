package daemon

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// A verbless approval: no permission verb is extracted, so the signature falls
// back to the trailing pane content (domain.salientContent) — the shape whose
// hash a repainting status line changes. `%s` carries that status line.
const jitterApprovalPaneTemplate = "The previous step finished and the summary is above. " +
	"It processed the remaining files and reported no problems. " +
	"It processed the remaining files and reported no problems. " +
	"It processed the remaining files and reported no problems. " +
	"It processed the remaining files and reported no problems. " +
	"It processed the remaining files and reported no problems. " +
	"It processed the remaining files and reported no problems.\n" +
	"Waiting for your approval to overwrite the generated file.\n" +
	"❯ 1. Yes\n  2. No\n%s\n"

func jitterApprovalPane(statusLine string) string {
	return fmt.Sprintf(jitterApprovalPaneTemplate, statusLine)
}

// consultCfg pins approval confidence high enough that a fully-agreeing history
// still takes the consult path, and the LLM auto-act threshold low enough that
// the returned score promotes.
const consultCfg = "[llm]\ncommand = [\"fake\"]\nauto_act_confidence_threshold = 50\ntimeout_seconds = 5\n" +
	"[confidence_thresholds]\napproval = 1.5\n"

// stageConsult returns a consult hook that stages a real decision row (as the
// production path requires) and repaints the pane to `paneDuring` first, so the
// post-consult staleness re-read sees a pane that moved while the LLM ran.
func stageConsult(h *harness, action, paneDuring string) func(context.Context, domain.LLMRequest) (*domain.LLMDecision, error) {
	return func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		h.herdr.setPane(paneDuring)
		id, err := h.raw.InsertLLMDecision(ctx, domain.LLMDecision{
			RequestID: req.RequestID, Signature: req.Signature,
			SituationType: req.SituationType, AgentType: req.AgentType,
			Action: action, Rationale: "matches the operator's usual answer",
			ConfidentScore: 90, Status: "pending", CreatedAt: time.Now(),
		})
		if err != nil {
			return nil, err
		}
		return &domain.LLMDecision{
			ID: id, RequestID: req.RequestID, Action: action,
			Rationale: "matches the operator's usual answer", ConfidentScore: 90, Status: "pending",
		}, nil
	}
}

// A confident LLM answer must survive the agent CLI repainting its status line
// while the consult runs: the situation is unchanged, so escalating it strands
// the agent for nothing.
func TestLLMConsultToleratesStatusLineJitter(t *testing.T) {
	before := jitterApprovalPane("esc to interrupt · 812 tokens · 41% context")
	during := jitterApprovalPane("esc to interrupt · 947 tokens · 43% context")

	// Premise: the two panes are the same situation but do NOT hash equal, so
	// only the jitter tolerance can carry this test.
	sigBefore := domain.ComputeSignature(classifierForTest().Classify("claude", "blocked", before))
	sigDuring := domain.ComputeSignature(classifierForTest().Classify("claude", "blocked", during))
	if sigBefore.Verdict != domain.GuardOK || sigDuring.Verdict != domain.GuardOK {
		t.Fatalf("premise broken: verdicts %v / %v", sigBefore.Verdict, sigDuring.Verdict)
	}
	if sigBefore.Raw == sigDuring.Raw {
		t.Fatal("premise broken: the repainted pane must hash differently")
	}

	h := newHarness(t, consultCfg)
	h.herdr.setPane(before)
	h.seedAutonomous(before, domain.SituationApproval, "1")
	h.llm.configured = true
	h.llm.consult = stageConsult(h, "1", during)

	h.push("agent-jitter", "blocked")
	waitFor(t, 5*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	if got := h.herdr.sentInputs()[0]; got != "1" {
		t.Errorf("delivered %q, want the promoted action \"1\"", got)
	}
	esc, _ := h.raw.PendingEscalations(context.Background())
	if len(esc) != 0 {
		t.Errorf("a pane that only repainted must not escalate, got %+v", esc)
	}
}

// The tolerance must not blur a pane that genuinely moved on: a different
// screen still escalates instead of receiving the staged answer.
func TestLLMConsultRejectsChangedPaneBeyondJitter(t *testing.T) {
	before := jitterApprovalPane("esc to interrupt · 812 tokens · 41% context")
	during := "Waiting for your approval to delete the release branch and force-push the rewritten history.\n" +
		"This is a different question about a different operation entirely, asked after the first resolved.\n" +
		"❯ 1. Yes\n  2. No\n"

	h := newHarness(t, consultCfg)
	h.herdr.setPane(before)
	h.seedAutonomous(before, domain.SituationApproval, "1")
	h.llm.configured = true
	h.llm.consult = stageConsult(h, "1", during)

	ctx := context.Background()
	h.push("agent-moved", "blocked")
	var esc []domain.AuditRecord
	waitFor(t, 5*time.Second, func() bool {
		esc, _ = h.raw.PendingEscalations(ctx)
		return len(esc) == 1
	})
	if sent := h.herdr.sentInputs(); len(sent) != 0 {
		t.Errorf("nothing may be delivered into a pane that moved on, sent %v", sent)
	}
	if !strings.Contains(esc[0].Rationale, string(domain.ReasonLLMNoSubmit)) {
		t.Errorf("want an %s escalation, got rationale %q", domain.ReasonLLMNoSubmit, esc[0].Rationale)
	}
}

// The tolerance's real adversary is not a whole new screen but the SAME
// scrollback with the decision line swapped — the shape whose shared text
// flatters the similarity metric. This pins the constant: it must sit below
// that pair's distance (measured ~21% at the default 500-rune salient window)
// and above an ordinary status-line repaint (~9-13%).
func TestLLMConsultRejectsSwappedQuestionOnSharedScrollback(t *testing.T) {
	status := "esc to interrupt · 812 tokens · 41% context"
	before := jitterApprovalPane(status)
	during := strings.Replace(before,
		"Waiting for your approval to overwrite the generated file.",
		"Waiting for your approval to publish the release to the public registry.", 1)

	h := newHarness(t, consultCfg)
	h.herdr.setPane(before)
	h.seedAutonomous(before, domain.SituationApproval, "1")
	h.llm.configured = true
	h.llm.consult = stageConsult(h, "1", during)

	ctx := context.Background()
	var esc []domain.AuditRecord
	h.push("agent-swapped", "blocked")
	waitFor(t, 5*time.Second, func() bool {
		esc, _ = h.raw.PendingEscalations(ctx)
		return len(esc) == 1
	})
	if sent := h.herdr.sentInputs(); len(sent) != 0 {
		t.Errorf("a different question must never receive the staged digit, sent %v", sent)
	}
	if !strings.Contains(esc[0].Rationale, string(domain.ReasonLLMNoSubmit)) {
		t.Errorf("want an %s escalation, got rationale %q", domain.ReasonLLMNoSubmit, esc[0].Rationale)
	}
}
