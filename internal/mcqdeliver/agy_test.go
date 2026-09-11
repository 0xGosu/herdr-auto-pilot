package mcqdeliver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// agyFixture loads one of the captured agy screens the classifier is pinned
// against, so these tests press keys into the real renders.
func agyFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "classify", "testdata", "transcripts", name+".txt"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// agyScreen is a pane that repaints when a scripted key arrives: each step
// names the key that moves it on and the screen that follows. A key that is
// not next in the script leaves the screen as it was — a form that ignores a
// key, which is exactly what delivery must notice.
type agyScreen struct {
	screen string
	steps  []agyStep
	keys   []string
}

type agyStep struct{ key, next string }

func (s *agyScreen) SendKey(_ context.Context, _, key string) error {
	s.keys = append(s.keys, key)
	if len(s.steps) > 0 && s.steps[0].key == key {
		s.screen = s.steps[0].next
		s.steps = s.steps[1:]
	}
	return nil
}

func (s *agyScreen) read(context.Context, string, int) (string, error) { return s.screen, nil }

func (s *agyScreen) cfg() Config {
	return Config{Keys: s, Read: s.read, PaneID: "w1:p1", ReadLines: 40, KeyDelay: time.Nanosecond}
}

// A numbered agy form is answered by the option's digit and nothing else: the
// digit commits, so an Enter after it would answer whatever screen came next.
func TestAgyAnswersANumberedFormWithTheDigitAlone(t *testing.T) {
	for _, tc := range []struct {
		name, fixture, reply, key, next string
	}{
		{"shell approval by label", "approval_agy_shell", "Yes, run command", "1", "idle_agy_after_turn"},
		{"shell approval by digit", "approval_agy_shell", "4", "4", "idle_agy_declined"},
		{"file access", "approval_agy_file_access", "No, deny access", "3", "idle_agy_declined"},
		{"file creation", "approval_agy_file_create", "Yes, allow creation", "1", "working_agy_spinner"},
		{"question 1 of 2 advances", "choice_agy_mcq_two", "Blue", "3", "choice_agy_mcq_two_q2"},
		{"last question submits", "choice_agy_mcq_two_q2", "Cat", "1", "idle_agy_after_turn"},
		{"review panel approve", "approval_agy_artifact_review", "approve", "y", "working_agy_spinner"},
		{"review panel reject", "approval_agy_artifact_review", "reject", "n", "idle_agy_after_turn"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pane := agyFixture(t, tc.fixture)
			s := &agyScreen{screen: pane, steps: []agyStep{{tc.key, agyFixture(t, tc.next)}}}
			if err := Agy(context.Background(), s.cfg(), pane, tc.reply); err != nil {
				t.Fatalf("Agy: %v", err)
			}
			if want := []string{tc.key}; !reflect.DeepEqual(s.keys, want) {
				t.Errorf("keys = %v, want exactly %v — never an Enter after a committing key", s.keys, want)
			}
		})
	}
}

// A key the form does not visibly take is an error — and it is NEVER pressed a
// second time: at a numbered agy form, a repeated digit would answer whatever
// screen the first one opened.
func TestAgyNeverRepeatsAKeyTheFormDidNotTake(t *testing.T) {
	pane := agyFixture(t, "approval_agy_shell")
	s := &agyScreen{screen: pane} // repaints on nothing
	err := Agy(context.Background(), s.cfg(), pane, "Yes, run command")
	if err == nil || !strings.Contains(err.Error(), "did not take") {
		t.Fatalf("err = %v, want a did-not-take refusal", err)
	}
	if want := []string{"1"}; !reflect.DeepEqual(s.keys, want) {
		t.Errorf("keys = %v, want exactly %v", s.keys, want)
	}
}

// The live form must be the one the answer was decided for. The second
// question of a form stands where the first stood; answering it with the first
// question's decision is the unseen answer the whole deliverer exists to stop.
func TestAgyRefusesAFormThatIsNotTheDecidedOne(t *testing.T) {
	s := &agyScreen{screen: agyFixture(t, "choice_agy_mcq_two_q2")}
	err := Agy(context.Background(), s.cfg(), agyFixture(t, "choice_agy_mcq_two"), "1")
	if err == nil || !strings.Contains(err.Error(), "not the one") {
		t.Fatalf("err = %v, want a stale-form refusal", err)
	}
	if len(s.keys) != 0 {
		t.Errorf("keys = %v, want none", s.keys)
	}
}

// Everything refused before the first key.
func TestAgyRefusesBeforeAnyKey(t *testing.T) {
	for _, tc := range []struct {
		name, screen, reply, want string
		notAnswerable             bool
	}{
		{name: "a label no option carries", screen: "approval_agy_shell", reply: "Yes, allow everything", want: "matches none"},
		{name: "the Write-in row by label", screen: "choice_agy_mcq", reply: "Write-in...", notAnswerable: true},
		{name: "the Write-in row by digit", screen: "choice_agy_mcq", reply: "4", notAnswerable: true},
		{name: "the amend field is open", screen: "approval_agy_shell_amend", reply: "1", want: "no longer showing"},
		{name: "a picker", screen: "idle_agy_model_picker", reply: "1", want: "no longer showing"},
		{name: "a ready composer", screen: "idle_agy_after_turn", reply: "1", want: "no longer showing"},
		{name: "a review reply that is neither", screen: "approval_agy_artifact_review", reply: "maybe later", want: "neither"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pane := agyFixture(t, tc.screen)
			s := &agyScreen{screen: pane}
			err := Agy(context.Background(), s.cfg(), "", tc.reply)
			switch {
			case err == nil:
				t.Fatal("delivery must be refused")
			case tc.notAnswerable && !errors.Is(err, domain.ErrAgyNotAnswerable):
				t.Errorf("err = %v, want ErrAgyNotAnswerable", err)
			case !tc.notAnswerable && !strings.Contains(err.Error(), tc.want):
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
			if len(s.keys) != 0 {
				t.Errorf("keys = %v, want none", s.keys)
			}
		})
	}
}

// The trust prompt's rows are unnumbered: the caret is walked to the chosen row
// — each press verified, since Enter commits whatever row the caret rests on —
// and only then confirmed.
func TestAgyTrustWalksTheCaretThenConfirms(t *testing.T) {
	onYes := agyFixture(t, "approval_agy_trust_folder")
	onNo := strings.Replace(onYes, "> Yes, I trust this folder\n  No, exit", "  Yes, I trust this folder\n> No, exit", 1)
	if onNo == onYes {
		t.Fatal("fixture drifted: could not move the caret")
	}
	gone := "root ➜ /tmp $ \n"

	t.Run("the row under the caret", func(t *testing.T) {
		s := &agyScreen{screen: onYes, steps: []agyStep{{"enter", agyFixture(t, "idle_agy_fresh")}}}
		if err := Agy(context.Background(), s.cfg(), onYes, "Yes, I trust this folder"); err != nil {
			t.Fatal(err)
		}
		if want := []string{"enter"}; !reflect.DeepEqual(s.keys, want) {
			t.Errorf("keys = %v, want %v", s.keys, want)
		}
	})
	t.Run("a row below it", func(t *testing.T) {
		s := &agyScreen{screen: onYes, steps: []agyStep{{"down", onNo}, {"enter", gone}}}
		if err := Agy(context.Background(), s.cfg(), onYes, "No, exit"); err != nil {
			t.Fatal(err)
		}
		if want := []string{"down", "enter"}; !reflect.DeepEqual(s.keys, want) {
			t.Errorf("keys = %v, want %v", s.keys, want)
		}
	})
	t.Run("a caret that does not move is never confirmed", func(t *testing.T) {
		s := &agyScreen{screen: onYes} // swallows the arrow
		err := Agy(context.Background(), s.cfg(), onYes, "No, exit")
		if err == nil || !strings.Contains(err.Error(), "did not reach") {
			t.Fatalf("err = %v, want a caret refusal", err)
		}
		if want := []string{"down"}; !reflect.DeepEqual(s.keys, want) {
			t.Errorf("keys = %v, want %v — Enter would commit the wrong row", s.keys, want)
		}
	})
}
