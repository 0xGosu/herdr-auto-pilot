package deliver_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/deliver"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// errRead / errSend are the induced adapter failures.
var (
	errRead = errors.New("induced read failure")
	errSend = errors.New("induced send failure")
)

// fakeHerdr is a plain HerdrPort: no keystrokes, no visible read. It is
// deliberately minimal so a test can prove the KEYSTROKE-LESS branches.
type fakeHerdr struct {
	pane    string
	inputs  []string
	readErr error
	sendErr error
}

func (f *fakeHerdr) Send(_ context.Context, _, input string) error {
	if f.sendErr != nil {
		return f.sendErr
	}
	f.inputs = append(f.inputs, input)
	return nil
}
func (f *fakeHerdr) ReadPane(context.Context, string, int) (string, error) {
	return f.pane, f.readErr
}
func (f *fakeHerdr) ListAgents(context.Context) ([]domain.AgentTransition, error) {
	return nil, nil
}

// fakeKeyHerdr adds SendKey, so it satisfies ports.KeystrokeSender. keyScript /
// keyScriptFrames swap the pane content when a scripted key arrives — delivery
// verifies every keystroke against the pane, so a fake whose content never
// reacts cannot deliver at all.
type fakeKeyHerdr struct {
	fakeHerdr
	keys            []string
	keyScript       []string
	keyScriptFrames []string
}

func (f *fakeKeyHerdr) SendKey(_ context.Context, _, key string) error {
	f.keys = append(f.keys, key)
	if len(f.keyScript) > 0 && len(f.keyScriptFrames) > 0 && key == f.keyScript[0] {
		f.keyScript = f.keyScript[1:]
		f.pane = f.keyScriptFrames[0]
		f.keyScriptFrames = f.keyScriptFrames[1:]
	}
	return nil
}

const remoteEnvPane = `   Select remote environment

   Configure environments at: https://claude.ai/code

   ❯ 1. herdr-auto-pilot (env_01F41H1jxkGrT2zj55CqE4WQ) ✔
     2. myspec-monorepo (env_01CASfztpZp7mYRJPK41sGvK)
     3. Full-access (env_011CUW5BKtc4vkq5q1uSp7MY)
     4. Default (env_011CUKn5Aj1q6ujg5PFvEhTE)

   Enter to select · Esc to cancel
`

const remoteEnvPaneCaret4 = `   Select remote environment

   Configure environments at: https://claude.ai/code

     1. herdr-auto-pilot (env_01F41H1jxkGrT2zj55CqE4WQ) ✔
     2. myspec-monorepo (env_01CASfztpZp7mYRJPK41sGvK)
     3. Full-access (env_011CUW5BKtc4vkq5q1uSp7MY)
   ❯ 4. Default (env_011CUKn5Aj1q6ujg5PFvEhTE)

   Enter to select · Esc to cancel
`

const approvalMenuPane = `Bash command

  npm test

Do you want to proceed?
1. Yes
2. No, and tell Claude what to do differently
`

// fastCfg keeps the keystroke pacing out of the test's wall clock. Production
// callers leave KeyDelay zero so it defaults to DefaultKeyDelay.
func fastCfg(h ports.HerdrPort) deliver.Config {
	return deliver.Config{Herdr: h, KeyDelay: time.Nanosecond}
}

// TestDeliverMapsMenuLabelToDigit pins the core reason delivery re-reads the
// pane at all: a numbered menu only accepts the option's digit, never the label.
func TestDeliverMapsMenuLabelToDigit(t *testing.T) {
	h := &fakeHerdr{pane: approvalMenuPane}
	err := deliver.Deliver(context.Background(), fastCfg(h), deliver.Request{
		PaneID: "w1:p1", AgentType: "claude",
		SituationType: domain.SituationApproval, Outbound: "Yes",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(h.inputs) != 1 || h.inputs[0] != "1" {
		t.Errorf("inputs = %v, want [1] (the digit, not the label)", h.inputs)
	}
}

// TestDeliverUnreadablePaneStillSendsLiteral proves the hoisted read's error is
// NOT fatal on the plain path: a free-text or unreadable situation delivers the
// literal reply unchanged.
func TestDeliverUnreadablePaneStillSendsLiteral(t *testing.T) {
	h := &fakeHerdr{readErr: errRead}
	err := deliver.Deliver(context.Background(), fastCfg(h), deliver.Request{
		PaneID: "w1:p1", AgentType: "claude",
		SituationType: domain.SituationApproval, Outbound: "Yes",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(h.inputs) != 1 || h.inputs[0] != "Yes" {
		t.Errorf("inputs = %v, want [Yes]", h.inputs)
	}
}

// TestDeliverSendFailure pins the bare error the frontend wraps with
// "correction recorded, but %w".
func TestDeliverSendFailure(t *testing.T) {
	h := &fakeHerdr{pane: approvalMenuPane, sendErr: errSend}
	err := deliver.Deliver(context.Background(), fastCfg(h), deliver.Request{
		PaneID: "w1:p1", AgentType: "claude",
		SituationType: domain.SituationApproval, Outbound: "Yes",
	})
	if err == nil || !strings.HasPrefix(err.Error(), "sending to the agent failed: ") {
		t.Fatalf("err = %v, want a bare \"sending to the agent failed: …\"", err)
	}
	if !errors.Is(err, errSend) {
		t.Errorf("the adapter error must stay unwrappable, got %v", err)
	}
}

// TestDeliverSeriesRefusals covers every answer-series refusal. All three must
// refuse BEFORE any keystroke, and none may fall through to a literal send —
// "1 2 1" typed as text would land in the first question's input.
func TestDeliverSeriesRefusals(t *testing.T) {
	tests := []struct {
		name    string
		herdr   func() *fakeKeyHerdr
		plain   bool // use the keystroke-LESS adapter
		wantErr string
	}{
		{
			name:    "unreadable pane",
			herdr:   func() *fakeKeyHerdr { return &fakeKeyHerdr{fakeHerdr: fakeHerdr{readErr: errRead}} },
			wantErr: "the pane could not be read to deliver the answer series: induced read failure",
		},
		{
			name:    "form no longer showing",
			herdr:   func() *fakeKeyHerdr { return &fakeKeyHerdr{fakeHerdr: fakeHerdr{pane: "just some output\n"}} },
			wantErr: "the pane no longer shows a 3-tab form; answer series not delivered",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := tt.herdr()
			err := deliver.Deliver(context.Background(), deliver.Config{Herdr: h, KeyDelay: time.Nanosecond},
				deliver.Request{
					PaneID: "w1:p1", SituationType: domain.SituationChoice, Outbound: "1 2 1",
				})
			if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
			if len(h.keys) != 0 {
				t.Errorf("refusal must happen before any keystroke, got %v", h.keys)
			}
			if len(h.inputs) != 0 {
				t.Errorf("a refused series must never fall through to a literal send: %v", h.inputs)
			}
		})
	}
}

// TestDeliverSeriesKeystrokelessAdapter pins the one refusal that depends on
// the adapter's capability rather than the pane. It reaches the check only with
// a form actually on screen, so it is separate from the table above.
func TestDeliverSeriesKeystrokelessAdapter(t *testing.T) {
	h := &fakeHerdr{pane: multiTabPane(3)}
	err := deliver.Deliver(context.Background(), fastCfg(h), deliver.Request{
		PaneID: "w1:p1", SituationType: domain.SituationChoice, Outbound: "1 2 1",
	})
	want := "this herdr adapter cannot send keystrokes for the answer series"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
	if len(h.inputs) != 0 {
		t.Errorf("a refused series must never fall through to a literal send: %v", h.inputs)
	}
}

// TestDeliverRemoteEnvAdaptive: a standing picker is answered with verified
// keystrokes, never a text send whose trailing Enter could commit whatever
// option the caret happens to rest on.
func TestDeliverRemoteEnvAdaptive(t *testing.T) {
	h := &fakeKeyHerdr{
		fakeHerdr:       fakeHerdr{pane: remoteEnvPane},
		keyScript:       []string{"4", "enter"},
		keyScriptFrames: []string{remoteEnvPaneCaret4, "● Launching remote agent…\n"},
	}
	err := deliver.Deliver(context.Background(), deliver.Config{Herdr: h, KeyDelay: time.Nanosecond},
		deliver.Request{
			PaneID: "w1:p1", AgentType: "claude", SituationType: domain.SituationApproval,
			PaneExcerpt: remoteEnvPane, Outbound: "Default (env_011CUKn5Aj1q6ujg5PFvEhTE)",
		})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(h.keys, ","); got != "4,enter" {
		t.Errorf("keys = %q, want \"4,enter\"", got)
	}
	if len(h.inputs) != 0 {
		t.Errorf("the picker must be answered with keystrokes, not a text send: %v", h.inputs)
	}
}

// TestDeliverRemoteEnvFailsClosed: a picker that cannot be read, or has already
// gone, refuses rather than falling through to the literal-label send.
func TestDeliverRemoteEnvFailsClosed(t *testing.T) {
	tests := []struct {
		name    string
		herdr   *fakeKeyHerdr
		wantErr string
	}{
		{
			name:    "unreadable",
			herdr:   &fakeKeyHerdr{fakeHerdr: fakeHerdr{readErr: errRead}},
			wantErr: "the pane could not be read to answer the remote environment picker: induced read failure",
		},
		{
			name:    "picker gone",
			herdr:   &fakeKeyHerdr{fakeHerdr: fakeHerdr{pane: "● Working…\n"}},
			wantErr: "the pane no longer shows the remote environment picker",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := deliver.Deliver(context.Background(), deliver.Config{Herdr: tt.herdr, KeyDelay: time.Nanosecond},
				deliver.Request{
					PaneID: "w1:p1", AgentType: "claude", SituationType: domain.SituationApproval,
					// The historical capture is what identifies the situation
					// when the live read fails or the picker has moved on.
					PaneExcerpt: remoteEnvPane, Outbound: "Default (env_011CUKn5Aj1q6ujg5PFvEhTE)",
				})
			if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
			if len(tt.herdr.keys) != 0 || len(tt.herdr.inputs) != 0 {
				t.Errorf("nothing may be delivered: keys=%v inputs=%v", tt.herdr.keys, tt.herdr.inputs)
			}
		})
	}
}

// TestDeliverRemoteEnvKeystrokelessFallsThrough is the branch-B → branch-C
// fallthrough. A keystroke-less adapter may still answer the picker, but ONLY
// when the reply maps to a live option digit — that digit is safe under both
// key bindings. Extracting branch B as a plain error-returning function would
// silently destroy this path, so it is pinned explicitly.
func TestDeliverRemoteEnvKeystrokelessFallsThrough(t *testing.T) {
	h := &fakeHerdr{pane: remoteEnvPane}
	err := deliver.Deliver(context.Background(), fastCfg(h), deliver.Request{
		PaneID: "w1:p1", AgentType: "claude", SituationType: domain.SituationApproval,
		PaneExcerpt: remoteEnvPane, Outbound: "Default (env_011CUKn5Aj1q6ujg5PFvEhTE)",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(h.inputs) != 1 || h.inputs[0] != "4" {
		t.Errorf("inputs = %v, want [4] (the mapped option digit)", h.inputs)
	}
}

// TestDeliverRemoteEnvKeystrokelessUnknownLabelFailsClosed is the other half of
// the fallthrough: an unmapped label has no safe digit, so it refuses.
func TestDeliverRemoteEnvKeystrokelessUnknownLabelFailsClosed(t *testing.T) {
	h := &fakeHerdr{pane: remoteEnvPane}
	err := deliver.Deliver(context.Background(), fastCfg(h), deliver.Request{
		PaneID: "w1:p1", AgentType: "claude", SituationType: domain.SituationApproval,
		PaneExcerpt: remoteEnvPane, Outbound: "Staging",
	})
	want := `"Staging" matches none of the offered environments and this herdr adapter cannot send verified keystrokes`
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
	if len(h.inputs) != 0 {
		t.Errorf("nothing may be sent: %v", h.inputs)
	}
}

// TestDeliverNonPickerApprovalIgnoresRemoteEnvBranch: an ordinary claude
// approval is not a picker, so the branch declines to handle it and the plain
// menu-digit path runs.
func TestDeliverNonPickerApprovalIgnoresRemoteEnvBranch(t *testing.T) {
	h := &fakeHerdr{pane: approvalMenuPane}
	err := deliver.Deliver(context.Background(), fastCfg(h), deliver.Request{
		PaneID: "w1:p1", AgentType: "claude", SituationType: domain.SituationApproval,
		PaneExcerpt: approvalMenuPane, Outbound: "Yes",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(h.inputs) != 1 || h.inputs[0] != "1" {
		t.Errorf("inputs = %v, want [1]", h.inputs)
	}
}

// multiTabPane renders a live Claude multi-tab question form of n tabs (Submit
// included) so the answer-count check passes and delivery reaches the
// capability assertion. Mirrors the rendering the frontend fakes use.
func multiTabPane(n int) string {
	marks := make([]string, 0, n-1)
	for i := 0; i < n-1; i++ {
		marks = append(marks, "☐ Q"+strconv.Itoa(i+1))
	}
	return "←  " + strings.Join(marks, "  ") + "  ✔ Submit  →\n\nQuestion 1?\n" +
		"❯ 1. sqlite\n  2. postgres\n\n" +
		"Enter to select · ↑/↓ to navigate · Tab to switch questions · Esc to cancel\n"
}
