package mcqdeliver

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// fakeClaudeQueue simulates Claude's paged permission dialog: requests queued
// behind one dialog, the header counting them ("1 of 3"), and an answered page
// replaced IN PLACE by the next — the shape #564 is about. answered records
// every request that got an answer, so a test can prove the trailing Enter did
// not approve a second one.
type fakeClaudeQueue struct {
	binding  digitBinding
	tools    []string // queued requests, head first
	caret    int
	answered []string
	keys     []string
	// lagReads is how many reads still show the answered page after a commit:
	// the redraw racing the deliverer's re-read.
	lagReads int
	lag      int
	stale    string
	// deaf ignores digits, so "the caret is not on the option" is reachable.
	deaf bool
	// noCounter drops the "1 of N" header counter, as Claude does on a narrow
	// row — two pages with identical bodies are then drawn identically.
	noCounter bool
	readErr   error
}

func newFakeClaudeQueue(binding digitBinding, tools ...string) *fakeClaudeQueue {
	return &fakeClaudeQueue{binding: binding, tools: tools, caret: 1}
}

func (f *fakeClaudeQueue) answer(option int) {
	f.stale = f.render()
	f.lag = f.lagReads
	f.answered = append(f.answered, fmt.Sprintf("%s=%d", f.tools[0], option))
	f.tools = f.tools[1:]
	f.caret = 1
}

func (f *fakeClaudeQueue) SendKey(_ context.Context, _, key string) error {
	f.keys = append(f.keys, key)
	if len(f.tools) == 0 {
		return nil
	}
	if key == "enter" {
		f.answer(f.caret)
		return nil
	}
	d, err := strconv.Atoi(key)
	if err != nil || d < 1 || d > 3 || f.deaf {
		return nil
	}
	if f.binding == digitCommits {
		f.answer(d)
		return nil
	}
	f.caret = d
	return nil
}

func (f *fakeClaudeQueue) Read(_ context.Context, _ string, _ int) (string, error) {
	if f.readErr != nil {
		return "", f.readErr
	}
	if f.lag > 0 {
		f.lag--
		return f.stale, nil
	}
	return f.render(), nil
}

func (f *fakeClaudeQueue) render() string {
	if len(f.tools) == 0 {
		return "● All three agents reported.\n\n" + strings.Repeat("─", 60) + "\n❯ \n" +
			strings.Repeat("─", 60) + "\n  repro | Opus | ⏸ manual mode on\n"
	}
	var b strings.Builder
	b.WriteString("● 3 general-purpose agents launched\n\n✻ Waiting for background agents\n\n")
	b.WriteString(strings.Repeat("─", 60) + "\n")
	if f.noCounter {
		b.WriteString(" Tool use · from the general-purpose agent\n")
	} else {
		fmt.Fprintf(&b, " Tool use · from the general-purpose agent             1 of %d\n", len(f.tools))
	}
	fmt.Fprintf(&b, " fakegh — %s Tool: (MCP)\n\n Do you want to proceed?\n", f.tools[0])
	for i, label := range []string{"Yes", "Yes, and don't ask again", "No"} {
		caret := " "
		if f.caret == i+1 {
			caret = "❯"
		}
		fmt.Fprintf(&b, " %s %d. %s\n", caret, i+1, label)
	}
	b.WriteString("\n Esc to cancel · Tab to amend\n")
	return b.String()
}

func (f *fakeClaudeQueue) config() Config {
	return Config{Keys: f, Read: f.Read, PaneID: "w1:p1", ReadLines: 40, KeyDelay: 0}
}

func noEnter(t *testing.T, keys []string) {
	t.Helper()
	for _, k := range keys {
		if k == "enter" {
			t.Fatalf("an Enter followed a digit that committed: keys %v", keys)
		}
	}
}

// The #564 shape: the digit commits page 1 and Claude draws page 2 in place.
// Exactly one request may be answered, and no Enter may follow.
func TestClaudeMenuDigitThatCommitsGetsNoEnter(t *testing.T) {
	lagging := func(reads int, tools ...string) *fakeClaudeQueue {
		f := newFakeClaudeQueue(digitCommits, tools...)
		f.lagReads = reads
		return f
	}
	narrow := func(tools ...string) *fakeClaudeQueue {
		f := newFakeClaudeQueue(digitCommits, tools...)
		f.noCounter = true
		return f
	}
	cases := []struct {
		name  string
		f     *fakeClaudeQueue
		digit string
	}{
		{"next request", newFakeClaudeQueue(digitCommits, "Get Pr", "Get Files", "Get Profile"), "2"},
		{"identical next page", newFakeClaudeQueue(digitCommits, "Get Pr", "Get Pr", "Get Pr"), "2"},
		{"redraw on the 3rd read", lagging(2, "Get Pr", "Get Files"), "2"},
		{"last page closes the dialog", newFakeClaudeQueue(digitCommits, "Get Pr"), "2"},
		// The caret already rests on option 1, which is also where the next
		// page puts it — so only the counter, or the rule that Enter needs a
		// caret MOVE, can tell the pages apart.
		{"Yes, identical next page", newFakeClaudeQueue(digitCommits, "Get Pr", "Get Pr", "Get Pr"), "1"},
		{"Yes, identical next page, no counter", narrow("Get Pr", "Get Pr"), "1"},
		{"Yes, redraw slower than the window", lagging(claudeMenuVerifyReads+1, "Get Pr", "Get Files"), "1"},
		{"No, redraw slower than the window", lagging(claudeMenuVerifyReads+1, "Get Pr", "Get Files"), "3"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := c.f
			if err := ClaudeMenu(context.Background(), f.config(), "", c.digit); err != nil {
				t.Fatalf("delivery failed: %v", err)
			}
			noEnter(t, f.keys)
			if !reflect.DeepEqual(f.keys, []string{c.digit}) {
				t.Errorf("keys = %v, want just the digit", f.keys)
			}
			if len(f.answered) != 1 || f.answered[0] != "Get Pr="+c.digit {
				t.Errorf("answered %v, want only page 1 with option %s", f.answered, c.digit)
			}
		})
	}
}

// The control half: a build where the digit only MOVES the caret still needs the
// Enter, pressed once the caret is verified on the chosen option.
func TestClaudeMenuDigitThatMovesTheCaretIsCommittedWithEnter(t *testing.T) {
	for _, digit := range []string{"2", "3"} {
		t.Run("option "+digit, func(t *testing.T) {
			f := newFakeClaudeQueue(digitMovesCaret, "Get Pr", "Get Files")
			before, err := ClaudeMenuPress(context.Background(), f.config(), "", digit)
			if err != nil {
				t.Fatal(err)
			}
			entered, err := ClaudeMenuSettle(context.Background(), f.config(), before, digit)
			if err != nil {
				t.Fatalf("delivery failed: %v", err)
			}
			if !entered || !reflect.DeepEqual(f.keys, []string{digit, "enter"}) {
				t.Errorf("entered=%v keys=%v, want the digit then Enter", entered, f.keys)
			}
			if !reflect.DeepEqual(f.answered, []string{"Get Pr=" + digit}) {
				t.Errorf("answered %v", f.answered)
			}
		})
	}
}

// A caret-only build answered with the option the caret already rests on shows
// no change at all — indistinguishable from a commit whose redraw is late, or
// from an identical next page. No Enter: the dialog is left standing for the
// self-check to report, never risked on the next request.
func TestClaudeMenuDigitAlreadyUnderTheCaretGetsNoEnter(t *testing.T) {
	f := newFakeClaudeQueue(digitMovesCaret, "Get Pr", "Get Files")
	if err := ClaudeMenu(context.Background(), f.config(), "", "1"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.keys, []string{"1"}) || len(f.answered) != 0 {
		t.Fatalf("keys = %v answered = %v, want the digit alone and nothing answered", f.keys, f.answered)
	}
}

// Once the digit is out, the settle step only decides about Enter: every doubt
// withholds it and none is a delivery failure — a failure would invite a retry
// that presses a digit into whatever Claude drew next. The self-check judges
// whether the answer landed.
func TestClaudeMenuWithholdsEnterOnAnyDoubt(t *testing.T) {
	t.Run("caret not on the chosen option", func(t *testing.T) {
		f := newFakeClaudeQueue(digitMovesCaret, "Get Pr")
		f.deaf = true
		if err := ClaudeMenu(context.Background(), f.config(), "", "2"); err != nil {
			t.Fatalf("err = %v", err)
		}
		noEnter(t, f.keys)
	})
	t.Run("pane unreadable after the digit", func(t *testing.T) {
		f := newFakeClaudeQueue(digitMovesCaret, "Get Pr")
		cfg := f.config()
		before, err := ClaudeMenuPress(context.Background(), cfg, "", "2")
		if err != nil {
			t.Fatal(err)
		}
		f.readErr = errors.New("herdr unreachable")
		if entered, err := ClaudeMenuSettle(context.Background(), cfg, before, "2"); entered || err != nil {
			t.Fatalf("entered=%v err=%v, want no Enter and no failure", entered, err)
		}
		noEnter(t, f.keys)
	})
}

func TestClaudeMenuRefusesBeforePressing(t *testing.T) {
	t.Run("option not offered", func(t *testing.T) {
		f := newFakeClaudeQueue(digitCommits, "Get Pr")
		err := ClaudeMenu(context.Background(), f.config(), "", "7")
		if err == nil || errors.Is(err, ErrNoClaudeMenu) {
			t.Fatalf("err = %v, want a refusal", err)
		}
		if len(f.keys) != 0 {
			t.Errorf("keys pressed: %v", f.keys)
		}
	})
	t.Run("read failure", func(t *testing.T) {
		f := newFakeClaudeQueue(digitCommits, "Get Pr")
		f.readErr = errors.New("herdr unreachable")
		if err := ClaudeMenu(context.Background(), f.config(), "", "1"); err == nil || errors.Is(err, ErrNoClaudeMenu) {
			t.Fatalf("err = %v, want a read failure", err)
		}
		if len(f.keys) != 0 {
			t.Errorf("keys pressed: %v", f.keys)
		}
	})
}

// No Claude dialog on screen, or one this deliverer does not model, presses
// nothing and hands the caller back its ordinary route.
func TestClaudeMenuWithoutADialogPressesNothing(t *testing.T) {
	f := newFakeClaudeQueue(digitCommits)
	if err := ClaudeMenu(context.Background(), f.config(), "", "1"); !errors.Is(err, ErrNoClaudeMenu) {
		t.Fatalf("err = %v, want ErrNoClaudeMenu", err)
	}
	if len(f.keys) != 0 {
		t.Errorf("keys pressed: %v", f.keys)
	}
}

// The dialog the answer was decided for was replaced in place before the digit
// went out — every Claude permission page offers the same options, so only the
// dialog itself can tell page 2 from page 1 (#571). Nothing may be pressed.
func TestClaudeMenuRefusesADialogItWasNotDecidedFor(t *testing.T) {
	f := newFakeClaudeQueue(digitCommits, "Get Files", "Get Profile")
	decided := newFakeClaudeQueue(digitCommits, "Get Pr", "Get Files", "Get Profile").render()
	if err := ClaudeMenu(context.Background(), f.config(), decided, "1"); !errors.Is(err, ErrClaudeMenuMoved) {
		t.Fatalf("err = %v, want ErrClaudeMenuMoved", err)
	}
	if len(f.keys) != 0 || len(f.answered) != 0 {
		t.Fatalf("keys = %v answered = %v; nothing may reach a dialog nobody decided about", f.keys, f.answered)
	}
}

// Only positive evidence of a DIFFERENT dialog refuses: the queue counter moving
// (another requester joined, or a page settled elsewhere) is the same dialog, and
// a decision screen with no dialog in it (the consuming capture often lacks the
// border) cannot be compared at all.
func TestClaudeMenuPressesWhenTheDialogIsTheSameOrUnknown(t *testing.T) {
	for name, decided := range map[string]string{
		"counter moved":        newFakeClaudeQueue(digitCommits, "Get Pr", "Get Files", "Get Profile", "Get Repo").render(),
		"no dialog in capture": "● 3 general-purpose agents launched\n\n✻ Waiting for background agents\n",
		"nothing kept":         "",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeClaudeQueue(digitCommits, "Get Pr", "Get Files", "Get Profile")
			if err := ClaudeMenu(context.Background(), f.config(), decided, "1"); err != nil {
				t.Fatalf("err = %v", err)
			}
			if !reflect.DeepEqual(f.answered, []string{"Get Pr=1"}) {
				t.Fatalf("answered %v, want page 1 answered", f.answered)
			}
		})
	}
}
