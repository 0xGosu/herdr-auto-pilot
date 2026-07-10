package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/control"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// A rule learned from a confirmed escalation must carry the real agent
// type, not "unknown": the escalation audit row records the type observed
// at classify time, and applyCorrection uses it when the signature has no
// prior history (the user-reported bug: idle rule saved with
// agent_type=unknown after accepting its escalation).
func TestConfirmedEscalationSavesRuleWithAgentType(t *testing.T) {
	dir := t.TempDir()
	taskFile := filepath.Join(dir, "tasks.md")
	os.WriteFile(taskFile, []byte("- [ ] update the docs\n"), 0o600)

	idlePane := "All tests pass. Task is complete.\n"
	cfg := fmt.Sprintf("[[task_sources]]\nagent = \"agent-at1\"\npath = %q\n", taskFile)
	h := newHarness(t, cfg)
	h.herdr.setPane(idlePane)

	// Fresh signature, no LLM: the idle situation escalates in shadow mode.
	h.push("agent-at1", "idle")
	ctx := context.Background()
	waitFor(t, 3*time.Second, func() bool {
		esc, _ := h.raw.PendingEscalations(ctx)
		return len(esc) == 1
	})
	esc, _ := h.raw.PendingEscalations(ctx)
	if esc[0].AgentType != "claude" {
		t.Fatalf("escalation audit agent type = %q, want %q", esc[0].AgentType, "claude")
	}

	// Operator confirms the suggested action.
	action := frontendSuggestedAction(esc[0])
	if action == "" {
		t.Fatalf("escalation carries no confirmable suggestion: %+v", esc[0])
	}
	if _, err := h.raw.InsertCorrection(ctx, domain.CorrectionRecord{
		AuditID: esc[0].ID, CorrectedAction: action, Author: "operator", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := control.Nudge(ctx, h.ctlPath, control.KindReload); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 3*time.Second, func() bool {
		st, _ := h.raw.GetSignature(ctx, esc[0].Signature)
		return st != nil
	})
	st, err := h.raw.GetSignature(ctx, esc[0].Signature)
	if err != nil || st == nil {
		t.Fatalf("signature state: %v %v", st, err)
	}
	if st.AgentType != "claude" {
		t.Errorf("rule saved with agent type %q, want %q", st.AgentType, "claude")
	}
	decs, _ := h.raw.DecisionsForSignature(ctx, esc[0].Signature, 10)
	if len(decs) == 0 || decs[0].AgentType != "claude" {
		t.Errorf("learned decision agent type wrong: %+v", decs)
	}
}

// Rules that already exist with agent_type unknown (learned before the
// audit log carried the type) heal on the next confirmation.
func TestConfirmationHealsUnknownAgentType(t *testing.T) {
	dir := t.TempDir()
	taskFile := filepath.Join(dir, "tasks.md")
	os.WriteFile(taskFile, []byte("- [ ] more work\n"), 0o600)

	idlePane := "All tests pass. Task is complete.\n"
	cfg := fmt.Sprintf("[[task_sources]]\nagent = \"agent-at2\"\npath = %q\n", taskFile)
	h := newHarness(t, cfg)
	h.herdr.setPane(idlePane)

	h.push("agent-at2", "idle")
	ctx := context.Background()
	waitFor(t, 3*time.Second, func() bool {
		esc, _ := h.raw.PendingEscalations(ctx)
		return len(esc) == 1
	})
	esc, _ := h.raw.PendingEscalations(ctx)

	// A legacy rule for this signature with the broken agent type.
	if err := h.raw.UpsertSignature(ctx, domain.SignatureState{
		Signature: esc[0].Signature, SituationType: domain.SituationIdle,
		AgentType: "unknown", Mode: domain.ModeShadow, UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	action := frontendSuggestedAction(esc[0])
	if _, err := h.raw.InsertCorrection(ctx, domain.CorrectionRecord{
		AuditID: esc[0].ID, CorrectedAction: action, Author: "operator", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := control.Nudge(ctx, h.ctlPath, control.KindReload); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 3*time.Second, func() bool {
		st, _ := h.raw.GetSignature(ctx, esc[0].Signature)
		return st != nil && st.AgentType == "claude"
	})
}

// frontendSuggestedAction mirrors frontend.SuggestedAction for the daemon
// tests (the frontend package is not imported here to keep the test local).
func frontendSuggestedAction(audit domain.AuditRecord) string {
	sug := audit.Suggestion
	for _, p := range []string{"respond: ", "choose: ", "on error: ", "LLM suggested: "} {
		if len(sug) > len(p) && sug[:len(p)] == p {
			return sug[len(p):]
		}
	}
	for _, p := range []string{"send next declared task: ", "send inferred next task: "} {
		if len(sug) > len(p) && sug[:len(p)] == p {
			if p == "send next declared task: " {
				return domain.ActionNextDeclaredTask
			}
			return domain.ActionNextInferredTask
		}
	}
	return sug
}
