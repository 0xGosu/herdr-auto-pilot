package daemon

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// Measured live: the LLM chose agy's scope-widening row at confidence 98-99 on
// nearly every approval. hap must answer the NARROW option instead — the
// widening one pre-authorises a command PREFIX for the rest of the
// conversation, so every later command matching it runs with no prompt at all
// and never reaches this screen, the classifier or the never-auto rules again.
//
// End to end through the promotion path: in agyApprovalPane option 2 is
// "Yes, and always allow …" and option 1 is "Yes, run command", so the digit
// that reaches the pane must be 1.
func TestAgyLLMWideningChoiceIsAnsweredWithTheNarrowOption(t *testing.T) {
	cfg := "[llm]\ncommand = [\"fake\"]\nauto_act_confidence_threshold = 50\ntimeout_seconds = 5\n"
	h := newHarness(t, cfg)
	h.herdr.setKeyScript(agyApprovalPane, []string{"1"}, []string{agyReadyPane})
	h.llm.configured = true
	const widening = "Yes, and always allow in this conversation for commands that start with 'go'"
	h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		id, _ := h.raw.InsertLLMDecision(ctx, domain.LLMDecision{
			RequestID: req.RequestID, Signature: req.Signature,
			SituationType: req.SituationType, AgentType: req.AgentType,
			Action: widening, Rationale: "the prefix looks safe", ConfidentScore: 99,
			Status: "pending", CreatedAt: time.Now(),
		})
		return &domain.LLMDecision{ID: id, RequestID: req.RequestID, Action: widening,
			Rationale: "the prefix looks safe", ConfidentScore: 99, Status: "pending"}, nil
	}

	h.pushAgy("agent-agy-widen", "done")

	if keys := waitForKeys(t, h, 1); !reflect.DeepEqual(keys, []string{"1"}) {
		t.Fatalf("keys = %v, want exactly [1] — the narrow grant, not the widening option the LLM chose", keys)
	}
	// The audit has to say the substitution happened, or the row reads as the
	// LLM having chosen the narrow option itself.
	rows, err := h.raw.AuditLog(context.Background(), 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if strings.Contains(r.Rationale, "substituted the narrowest option") {
			return
		}
	}
	t.Error("no audit row records that hap substituted the narrow option for the LLM's choice")
}

// The control: an LLM answer that is ALREADY the narrow option is delivered
// unchanged, and nothing is recorded as substituted.
func TestAgyLLMNarrowChoiceIsLeftAlone(t *testing.T) {
	cfg := "[llm]\ncommand = [\"fake\"]\nauto_act_confidence_threshold = 50\ntimeout_seconds = 5\n"
	h := newHarness(t, cfg)
	h.herdr.setKeyScript(agyApprovalPane, []string{"1"}, []string{agyReadyPane})
	h.llm.configured = true
	h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		const action = "Yes, run command"
		id, _ := h.raw.InsertLLMDecision(ctx, domain.LLMDecision{
			RequestID: req.RequestID, Signature: req.Signature,
			SituationType: req.SituationType, AgentType: req.AgentType,
			Action: action, Rationale: "read-only", ConfidentScore: 95,
			Status: "pending", CreatedAt: time.Now(),
		})
		return &domain.LLMDecision{ID: id, RequestID: req.RequestID, Action: action,
			Rationale: "read-only", ConfidentScore: 95, Status: "pending"}, nil
	}

	h.pushAgy("agent-agy-narrow", "done")

	if keys := waitForKeys(t, h, 1); !reflect.DeepEqual(keys, []string{"1"}) {
		t.Fatalf("keys = %v, want exactly [1]", keys)
	}
	rows, err := h.raw.AuditLog(context.Background(), 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if strings.Contains(r.Rationale, "substituted the narrowest option") {
			t.Error("an already-narrow answer must not be reported as substituted")
		}
	}
}
