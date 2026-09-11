package deliver_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/deliver"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

func agyFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "classify", "testdata", "transcripts", name+".txt"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// An agy approval is answered with the option's digit as a KEY and nothing
// else — never the generic send, whose trailing Enter would answer agy's next
// screen. The operator's --send and auto-accept both arrive here.
func TestDeliverAnswersAgyFormsWithTheKeyAlone(t *testing.T) {
	approval := agyFixture(t, "approval_agy_shell")
	h := &fakeKeyHerdr{fakeHerdr: fakeHerdr{pane: approval},
		keyScript: []string{"1"}, keyScriptFrames: []string{agyFixture(t, "idle_agy_after_turn")}}
	err := deliver.Deliver(context.Background(), fastCfg(h), deliver.Request{
		PaneID: "w1:p1", AgentType: "agy", SituationType: domain.SituationApproval,
		PaneExcerpt: approval, Outbound: "Yes, run command",
	})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if want := []string{"1"}; !reflect.DeepEqual(h.keys, want) || len(h.inputs) != 0 {
		t.Errorf("keys=%v inputs=%v, want keys %v and no submitted text", h.keys, h.inputs, want)
	}
}

// A question's Write-in row asks for typed text: a verdict about the form, so it
// is ErrReplyWithheld (auto-accept must not burn attempts on it), and nothing
// is typed.
func TestDeliverRefusesAnAgyWriteInAnswer(t *testing.T) {
	mcq := agyFixture(t, "choice_agy_mcq")
	h := &fakeKeyHerdr{fakeHerdr: fakeHerdr{pane: mcq}}
	err := deliver.Deliver(context.Background(), fastCfg(h), deliver.Request{
		PaneID: "w1:p1", AgentType: "agy", SituationType: domain.SituationChoice,
		PaneExcerpt: mcq, Outbound: "Write-in...",
	})
	if !errors.Is(err, deliver.ErrReplyWithheld) {
		t.Fatalf("err = %v, want ErrReplyWithheld", err)
	}
	if len(h.keys) != 0 || len(h.inputs) != 0 {
		t.Errorf("nothing may reach the pane, got keys=%v inputs=%v", h.keys, h.inputs)
	}
}

// There is no keystroke-less fallback for an agy form: every one needs a key
// that is not followed by Enter, which a plain Send cannot type.
func TestDeliverAgyFormNeedsKeystrokes(t *testing.T) {
	approval := agyFixture(t, "approval_agy_shell")
	h := &fakeHerdr{pane: approval}
	err := deliver.Deliver(context.Background(), fastCfg(h), deliver.Request{
		PaneID: "w1:p1", AgentType: "agy", SituationType: domain.SituationApproval,
		PaneExcerpt: approval, Outbound: "1",
	})
	if err == nil {
		t.Fatal("a keystroke-less adapter must refuse an agy form")
	}
	if len(h.inputs) != 0 {
		t.Errorf("inputs = %v, want none", h.inputs)
	}
}

// Free text for agy (an error reply, an idle hand-out) goes only into a proven
// EMPTY composer; the operator's draft, a form or a picker refuses it.
func TestDeliverAgyFreeTextNeedsAReadyComposer(t *testing.T) {
	for _, tc := range []struct {
		screen string
		ready  bool
	}{
		{"idle_agy_after_turn", true},
		{"idle_agy_mode_plan", true},
		{"idle_agy_composer_draft", false},
		{"idle_agy_model_picker", false},
		{"approval_agy_shell", false},
		{"working_agy_spinner", false},
	} {
		t.Run(tc.screen, func(t *testing.T) {
			h := &fakeHerdr{pane: agyFixture(t, tc.screen)}
			err := deliver.Deliver(context.Background(), fastCfg(h), deliver.Request{
				PaneID: "w1:p1", AgentType: "agy", SituationType: domain.SituationError, Outbound: "continue",
			})
			if tc.ready {
				if err != nil || !reflect.DeepEqual(h.inputs, []string{"continue"}) {
					t.Fatalf("err=%v inputs=%v, want the reply delivered", err, h.inputs)
				}
				return
			}
			if err == nil || len(h.inputs) != 0 {
				t.Fatalf("err=%v inputs=%v, want a refusal and nothing typed", err, h.inputs)
			}
		})
	}
}
