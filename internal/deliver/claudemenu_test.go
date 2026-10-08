package deliver_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/deliver"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/mcqdeliver"
)

// pagedApprovalPane renders Claude's paged permission dialog ("1 of N") for
// tool, with the caret on option caret.
func pagedApprovalPane(tool string, remaining, caret int) string {
	var b strings.Builder
	b.WriteString("● 3 general-purpose agents launched\n\n" + strings.Repeat("─", 60) + "\n")
	fmt.Fprintf(&b, " Tool use · from the general-purpose agent             1 of %d\n", remaining)
	fmt.Fprintf(&b, " fakegh — %s Tool: (MCP)\n\n Do you want to proceed?\n", tool)
	for i, label := range []string{"Yes", "Yes, and don't ask again", "No"} {
		mark := " "
		if caret == i+1 {
			mark = "❯"
		}
		fmt.Fprintf(&b, " %s %d. %s\n", mark, i+1, label)
	}
	b.WriteString("\n Esc to cancel · Tab to amend\n")
	return b.String()
}

func pagedRequest(outbound string) deliver.Request {
	return deliver.Request{
		PaneID: "w1:p1", AgentType: "claude", SituationType: domain.SituationApproval,
		PaneExcerpt: pagedApprovalPane("Get Pr", 3, 1), Outbound: outbound,
	}
}

// #564: Claude commits a permission dialog on the digit and draws the next
// queued request in place, so the answer is the digit KEY alone — an Enter after
// it would approve page 2 unseen. Also covers the identical-body page, where only
// the queue counter differs.
func TestDeliverClaudeMenuDigitIsAKeyWithNoEnter(t *testing.T) {
	for name, next := range map[string]string{
		"next request":        pagedApprovalPane("Get Files", 2, 1),
		"identical next page": pagedApprovalPane("Get Pr", 2, 1),
	} {
		t.Run(name, func(t *testing.T) {
			h := &fakeKeyHerdr{
				fakeHerdr: fakeHerdr{pane: pagedApprovalPane("Get Pr", 3, 1)},
				keyScript: []string{"2"}, keyScriptFrames: []string{next},
			}
			if err := deliver.Deliver(context.Background(), fastCfg(h), pagedRequest("Yes, and don't ask again")); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(h.keys, []string{"2"}) {
				t.Errorf("keys = %v, want just the digit", h.keys)
			}
			if len(h.inputs) != 0 {
				t.Errorf("typed %v; a menu digit must not go through the text send and its Enter", h.inputs)
			}
		})
	}
}

// The control half: a build whose digit only moves the caret gets its Enter.
func TestDeliverClaudeMenuDigitThatMovesTheCaretGetsEnter(t *testing.T) {
	h := &fakeKeyHerdr{
		fakeHerdr:       fakeHerdr{pane: pagedApprovalPane("Get Pr", 3, 1)},
		keyScript:       []string{"2", "enter"},
		keyScriptFrames: []string{pagedApprovalPane("Get Pr", 3, 2), pagedApprovalPane("Get Files", 2, 1)},
	}
	if err := deliver.Deliver(context.Background(), fastCfg(h), pagedRequest("Yes, and don't ask again")); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(h.keys, []string{"2", "enter"}) {
		t.Errorf("keys = %v, want the digit then Enter", h.keys)
	}
}

// A dialog that stays put with the caret elsewhere gets no Enter — it would
// commit the caret's option — and no failure: a failure would let auto-accept
// retry with another digit, and whether the answer landed is the self-check's
// call.
func TestDeliverClaudeMenuDigitWithholdsEnterWhenTheCaretIsElsewhere(t *testing.T) {
	h := &fakeKeyHerdr{fakeHerdr: fakeHerdr{pane: pagedApprovalPane("Get Pr", 3, 1)}}
	if err := deliver.Deliver(context.Background(), fastCfg(h), pagedRequest("Yes, and don't ask again")); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(h.keys, []string{"2"}) {
		t.Fatalf("keys = %v, want the digit and no Enter", h.keys)
	}
}

// Without keystrokes, or without a dialog the key deliverer models, the ordinary
// send is kept.
func TestDeliverClaudeMenuDigitFallsBackToTheTextSend(t *testing.T) {
	t.Run("keystroke-less adapter", func(t *testing.T) {
		h := &fakeHerdr{pane: pagedApprovalPane("Get Pr", 3, 1)}
		if err := deliver.Deliver(context.Background(), fastCfg(h), pagedRequest("Yes")); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(h.inputs, []string{"1"}) {
			t.Errorf("inputs = %v, want [1]", h.inputs)
		}
	})
	t.Run("no ruled dialog", func(t *testing.T) {
		h := &fakeKeyHerdr{fakeHerdr: fakeHerdr{pane: approvalMenuPane}}
		if err := deliver.Deliver(context.Background(), fastCfg(h), pagedRequest("Yes")); err != nil {
			t.Fatal(err)
		}
		if len(h.keys) != 0 || !reflect.DeepEqual(h.inputs, []string{"1"}) {
			t.Errorf("keys = %v inputs = %v, want the text send of [1]", h.keys, h.inputs)
		}
	})
}

// With SettleAsync, Deliver returns once the digit is pressed — its callers run
// on the daemon's select loop — and the settle that may add Enter runs through
// the hook instead.
func TestDeliverClaudeMenuDigitHandsTheSettleOff(t *testing.T) {
	h := &fakeKeyHerdr{
		fakeHerdr:       fakeHerdr{pane: pagedApprovalPane("Get Pr", 3, 1)},
		keyScript:       []string{"2", "enter"},
		keyScriptFrames: []string{pagedApprovalPane("Get Pr", 3, 2), pagedApprovalPane("Get Files", 2, 1)},
	}
	var settle func(context.Context)
	cfg := fastCfg(h)
	cfg.SettleAsync = func(s func(context.Context)) { settle = s }
	if err := deliver.Deliver(context.Background(), cfg, pagedRequest("Yes, and don't ask again")); err != nil {
		t.Fatal(err)
	}
	if settle == nil || !reflect.DeepEqual(h.keys, []string{"2"}) {
		t.Fatalf("settle handed off = %v, keys = %v; want the digit pressed and the settle deferred", settle != nil, h.keys)
	}
	settle(context.Background())
	if !reflect.DeepEqual(h.keys, []string{"2", "enter"}) {
		t.Fatalf("keys after the settle = %v, want the digit then Enter", h.keys)
	}
}

// An answer decided on page 1 is not pressed into page 2 (#571): an operator's
// reply, or an auto-accept, whose dialog was replaced in place is refused with
// nothing typed.
func TestDeliverClaudeMenuDigitRefusesADialogItWasNotDecidedFor(t *testing.T) {
	h := &fakeKeyHerdr{fakeHerdr: fakeHerdr{pane: pagedApprovalPane("Get Files", 2, 1)}}
	err := deliver.Deliver(context.Background(), fastCfg(h), pagedRequest("Yes"))
	if !errors.Is(err, mcqdeliver.ErrClaudeMenuMoved) {
		t.Fatalf("err = %v, want ErrClaudeMenuMoved", err)
	}
	if len(h.keys) != 0 || len(h.inputs) != 0 {
		t.Fatalf("keys = %v inputs = %v; nothing may be typed", h.keys, h.inputs)
	}
}
