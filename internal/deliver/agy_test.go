package deliver_test

import (
	"context"
	"errors"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/deliver"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// agyShellApprovalPane is agy 1.2.1's shell approval as it stands in a pane.
const agyShellApprovalPane = "Requesting permission for:\n   go test ./...\n\nRun this command?\n" +
	"> 1. Yes, run command\n" +
	"  2. Yes, and always allow in this conversation for commands that start with 'go'\n" +
	"  3. Yes, and always allow for commands that start with 'go' (Persist to settings.json)\n" +
	"  4. No, cancel\n\n" +
	"  ↑/↓ Navigate · tab Amend · ctrl+g edit/expand command\n" +
	"esc to cancel                                              Gemini 3.6 Flash · low\n"

// TestDeliverWithholdsAgyForms: at an agy menu the digit alone commits, so the
// generic send (digit, then Enter) would answer agy's NEXT screen as well.
// Until hap speaks agy's protocol, every approval and choice reply is refused
// before anything is typed — a label, a digit and an answer series alike.
func TestDeliverWithholdsAgyForms(t *testing.T) {
	for _, st := range []domain.SituationType{domain.SituationApproval, domain.SituationChoice} {
		for _, outbound := range []string{"Yes, run command", "1", "3 1"} {
			h := &fakeKeyHerdr{fakeHerdr: fakeHerdr{pane: agyShellApprovalPane}}
			err := deliver.Deliver(context.Background(), fastCfg(h), deliver.Request{
				PaneID: "w1:p1", AgentType: "agy", SituationType: st,
				PaneExcerpt: agyShellApprovalPane, Outbound: outbound,
			})
			if !errors.Is(err, deliver.ErrReplyWithheld) {
				t.Errorf("%s %q: err = %v, want ErrReplyWithheld", st, outbound, err)
			}
			if len(h.inputs) != 0 || len(h.keys) != 0 {
				t.Errorf("%s %q: nothing may reach the pane, got inputs=%v keys=%v", st, outbound, h.inputs, h.keys)
			}
		}
	}
}

// Control: an agy error reply is free text typed into the composer — what agy
// expects there — so it is delivered exactly as for any other agent.
func TestDeliverStillSendsAgyErrorReplies(t *testing.T) {
	h := &fakeHerdr{pane: "  ⎿  Interrupted · What should Antigravity CLI do instead?\n"}
	err := deliver.Deliver(context.Background(), fastCfg(h), deliver.Request{
		PaneID: "w1:p1", AgentType: "agy", SituationType: domain.SituationError, Outbound: "continue",
	})
	if err != nil {
		t.Fatalf("an error reply must be delivered: %v", err)
	}
	if len(h.inputs) != 1 || h.inputs[0] != "continue" {
		t.Errorf("inputs = %v, want [continue]", h.inputs)
	}
}
