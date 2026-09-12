package classify

import (
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// herdr reports EVERY agy modal as idle/done, so on an agy pane a parked status
// says nothing about whether anything is on screen. Falling through to the idle
// verdict is what let hap offer the next task to an agent sat behind an
// edit-approval prompt for ten minutes — and a pane scored idle is exactly the
// state in which a hand-out is typed into a live modal.
//
// The rule is structural on purpose: agy must SHOW its composer to earn the
// idle verdict. A list of known modals would not have helped, because the modal
// that caused this was one no parser recognised.

const agyRule = "────────────────────────────────────────────────────────────"

func agyComposerPane(caret string) string {
	return strings.Join([]string{
		"● Bash(go test ./...) (ctrl+o to expand)",
		"",
		"  All done.",
		"",
		agyRule,
		caret,
		agyRule,
		"? for shortcuts                                  Gemini 3.6 Flash · low",
	}, "\n") + "\n"
}

// An unrecognised modal covering the composer. Deliberately NOT presented as a
// recorded agy screen: it stands for "some form hap cannot parse", which is the
// population this rule exists for. The reported edit-accept modal is one member
// of it, and is not reproduced verbatim here because no capture of it exists.
const agyUnknownModalPane = "● Edit(main.go) (ctrl+o to expand)\n\n" +
	"  shift+tab to auto-approve file edits\n" +
	"Accept this file edit?\n" +
	"> 1. Yes, accept this change\n" +
	"  2. No, reject this change\n"

func TestAgyUnknownModalSuppressesTheIdleVerdict(t *testing.T) {
	c := New(nil)
	for _, status := range []string{"idle", "done"} {
		s := c.Classify(domain.AgentTypeAgy, status, agyUnknownModalPane)
		if s.Type != domain.SituationUnclassifiable {
			t.Errorf("@%s: type = %s, want unclassifiable — a modal hap cannot read must "+
				"never be reported as a parked agent free to take the next task", status, s.Type)
		}
	}
}

// The control, and the reason this is not simply "agy is never idle": a parked
// composer must still earn the idle verdict, or the suppression kills every agy
// hand-out instead of fixing a stall.
func TestAgyParkedComposerStillClassifiesIdle(t *testing.T) {
	c := New(nil)
	cases := []struct{ name, caret string }{
		{"an empty composer", ">"},
		// A draft is NOT a modal: the agent is genuinely parked, the operator is
		// simply mid-sentence. Keying this rule on composer READINESS instead of
		// visibility would escalate every draft an operator leaves behind.
		{"a half-typed draft", "> half a thought"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, status := range []string{"idle", "done"} {
				if s := c.Classify(domain.AgentTypeAgy, status, agyComposerPane(tc.caret)); s.Type != domain.SituationIdle {
					t.Errorf("@%s: type = %s, want idle", status, s.Type)
				}
			}
		})
	}
}

// The suppression is agy-only: claude and codex report a modal as blocked, so
// their idle really does mean idle and gating it would strand them.
func TestIdleSuppressionIsAgyOnly(t *testing.T) {
	c := New(nil)
	for _, agent := range []string{"claude", "codex"} {
		if s := c.Classify(agent, "idle", "  Task finished.\n"); s.Type != domain.SituationIdle {
			t.Errorf("%s: type = %s, want idle", agent, s.Type)
		}
	}
}

// A working agy agent is unaffected either way: the idle tail is never reached
// at a non-parked status.
func TestAgyWorkingStatusIsUnchangedBySuppression(t *testing.T) {
	c := New(nil)
	if s := c.Classify(domain.AgentTypeAgy, "working", agyUnknownModalPane); s.Type != domain.SituationUnclassifiable {
		t.Errorf("type = %s, want unclassifiable (unchanged)", s.Type)
	}
}
