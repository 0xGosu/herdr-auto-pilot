package mcqdeliver

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// fakeRemoteEnvMenu simulates Claude's standing "Select remote environment"
// picker. Like fakeForm it renders the real on-screen shape so the production
// domain parser is what reads it, and it supports both digit bindings because
// the real picker's commit protocol is unverified.
type fakeRemoteEnvMenu struct {
	binding  digitBinding
	labels   []string
	caret    int // 1-based option under the caret
	selected string
	closed   bool
	keys     []string
	failKey  string
	// stuck models a picker that ignores Enter, so the fail-closed guard for
	// "did not commit" is reachable.
	stuck bool
}

func newFakeRemoteEnvMenu(binding digitBinding) *fakeRemoteEnvMenu {
	return &fakeRemoteEnvMenu{
		binding: binding,
		labels: []string{
			"herdr-auto-pilot (env_01F41H1jxkGrT2zj55CqE4WQ) ✔",
			"myspec-monorepo (env_01CASfztpZp7mYRJPK41sGvK)",
			"Full-access (env_011CUW5BKtc4vkq5q1uSp7MY)",
			"Default (env_011CUKn5Aj1q6ujg5PFvEhTE)",
		},
		caret: 1,
	}
}

func (f *fakeRemoteEnvMenu) SendKey(_ context.Context, _, key string) error {
	if key == f.failKey {
		return errors.New("induced keystroke failure")
	}
	f.keys = append(f.keys, key)
	if f.closed {
		return nil // keys land in the composer once the picker is gone
	}
	switch key {
	case "enter":
		if !f.stuck {
			f.selected = f.labels[f.caret-1]
			f.closed = true
		}
	default:
		d, err := strconv.Atoi(key)
		if err != nil || d < 1 || d > len(f.labels) {
			break
		}
		f.caret = d
		if f.binding == digitCommits {
			f.selected = f.labels[d-1]
			f.closed = true
		}
	}
	return nil
}

func (f *fakeRemoteEnvMenu) Read(_ context.Context, _ string, _ int) (string, error) {
	if f.closed {
		return "● Environment selected. Launching remote agent…\n", nil
	}
	var b strings.Builder
	b.WriteString("   Select remote environment\n\n")
	b.WriteString("   Configure environments at: https://claude.ai/code\n\n")
	for i, label := range f.labels {
		caret := " "
		if f.caret == i+1 {
			caret = "❯"
		}
		fmt.Fprintf(&b, "   %s %d. %s\n", caret, i+1, label)
	}
	b.WriteString("\n   Enter to select · Esc to cancel\n")
	return b.String(), nil
}

func (f *fakeRemoteEnvMenu) config() Config {
	return Config{Keys: f, Read: f.Read, PaneID: "w1:p1", ReadLines: 40, KeyDelay: 0}
}

// The picker's commit protocol is unverified, so delivery must land the
// selection under BOTH real Claude bindings without knowing which is live.
func TestClaudeRemoteEnvAnswersBothBindings(t *testing.T) {
	tests := []struct {
		name     string
		binding  digitBinding
		chosen   string
		wantKeys []string
		wantSel  string
	}{
		{
			name:     "digit commits on its own",
			binding:  digitCommits,
			chosen:   "Full-access (env_011CUW5BKtc4vkq5q1uSp7MY)",
			wantKeys: []string{"3"},
			wantSel:  "Full-access (env_011CUW5BKtc4vkq5q1uSp7MY)",
		},
		{
			name:     "digit only moves the caret, Enter commits",
			binding:  digitMovesCaret,
			chosen:   "Default (env_011CUKn5Aj1q6ujg5PFvEhTE)",
			wantKeys: []string{"4", "enter"},
			wantSel:  "Default (env_011CUKn5Aj1q6ujg5PFvEhTE)",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			menu := newFakeRemoteEnvMenu(tc.binding)
			if err := ClaudeRemoteEnv(context.Background(), menu.config(), tc.chosen); err != nil {
				t.Fatalf("ClaudeRemoteEnv returned error: %v", err)
			}
			if strings.Join(menu.keys, ",") != strings.Join(tc.wantKeys, ",") {
				t.Errorf("keys sent = %v, want %v", menu.keys, tc.wantKeys)
			}
			if menu.selected != tc.wantSel {
				t.Errorf("selected = %q, want %q", menu.selected, tc.wantSel)
			}
		})
	}
}

// The default entry renders with a trailing ✔ but the learned label is clean;
// both directions must resolve to the digit (the marker is UI state).
func TestClaudeRemoteEnvResolvesCheckMarkedLabel(t *testing.T) {
	for _, chosen := range []string{
		"herdr-auto-pilot (env_01F41H1jxkGrT2zj55CqE4WQ)",
		"herdr-auto-pilot (env_01F41H1jxkGrT2zj55CqE4WQ) ✔",
		"1",
	} {
		menu := newFakeRemoteEnvMenu(digitMovesCaret)
		if err := ClaudeRemoteEnv(context.Background(), menu.config(), chosen); err != nil {
			t.Fatalf("chosen %q: %v", chosen, err)
		}
		if !strings.HasPrefix(menu.selected, "herdr-auto-pilot") {
			t.Errorf("chosen %q selected %q", chosen, menu.selected)
		}
	}
}

// Selecting the option the caret already rests on (the ✔-marked default is
// also option 1, where the caret starts) needs no special case under either
// binding.
func TestClaudeRemoteEnvSelectsOptionAlreadyUnderCaret(t *testing.T) {
	for _, binding := range []digitBinding{digitCommits, digitMovesCaret} {
		menu := newFakeRemoteEnvMenu(binding)
		if err := ClaudeRemoteEnv(context.Background(), menu.config(), "1"); err != nil {
			t.Fatalf("binding %v: %v", binding, err)
		}
		if !menu.closed || !strings.HasPrefix(menu.selected, "herdr-auto-pilot") {
			t.Errorf("binding %v: closed=%v selected=%q", binding, menu.closed, menu.selected)
		}
	}
}

// The cross-project backstop: the picker's approval signature is global, so a
// rule learned on another project's environment list can resolve here. When
// its label matches none of the offered environments, delivery must refuse
// BEFORE any keystroke — never commit whatever the caret rests on.
func TestClaudeRemoteEnvRefusesUnknownLabelBeforeAnyKeystroke(t *testing.T) {
	menu := newFakeRemoteEnvMenu(digitMovesCaret)
	err := ClaudeRemoteEnv(context.Background(), menu.config(), "other-project (env_01ZZZZZZZZZZZZZZZZZZZZZZZZ)")
	if err == nil {
		t.Fatal("expected an error for a label absent from the live menu")
	}
	if !strings.Contains(err.Error(), "none of the offered environments") {
		t.Errorf("error = %v, want it to name the unmatched label", err)
	}
	if len(menu.keys) != 0 {
		t.Fatalf("no keystroke may be sent for an unmappable label, got %v", menu.keys)
	}
}

// Fail closed when the digit neither commits nor moves the caret to the
// chosen option: Enter must NOT be pressed.
func TestClaudeRemoteEnvRefusesWhenCaretDidNotReachOption(t *testing.T) {
	menu := newFakeRemoteEnvMenu(digitMovesCaret)
	// keyDropper records digit keys without applying them, so the caret
	// never reaches the chosen option.
	cfg := menu.config()
	cfg.Keys = keyDropper{menu: menu}
	err := ClaudeRemoteEnv(context.Background(), cfg, "3")
	if err == nil {
		t.Fatal("expected an error when the caret never reached the chosen option")
	}
	if !strings.Contains(err.Error(), "was not selected") {
		t.Errorf("error = %v, want it to name the unselected option", err)
	}
	for _, k := range menu.keys {
		if k == "enter" {
			t.Fatal("Enter must not be pressed once the caret is known to be wrong")
		}
	}
	if menu.closed {
		t.Error("nothing should have been committed")
	}
}

// keyDropper records digit keys without applying them (a build that unbinds
// digits) while still letting Enter through to the underlying menu.
type keyDropper struct{ menu *fakeRemoteEnvMenu }

func (k keyDropper) SendKey(ctx context.Context, pane, key string) error {
	if key == "enter" {
		return k.menu.SendKey(ctx, pane, key)
	}
	k.menu.keys = append(k.menu.keys, key)
	return nil
}

// A picker that ignores Enter (or re-renders unchanged) must surface as a
// failed commit, not report success.
func TestClaudeRemoteEnvRefusesWhenPickerDoesNotCommit(t *testing.T) {
	menu := newFakeRemoteEnvMenu(digitMovesCaret)
	menu.stuck = true
	err := ClaudeRemoteEnv(context.Background(), menu.config(), "2")
	if err == nil || !strings.Contains(err.Error(), "did not commit") {
		t.Fatalf("error = %v, want the stuck picker to surface", err)
	}
}

// The picker being gone before delivery starts (already answered, or the
// agent moved on) must fail closed instead of typing into whatever is there.
func TestClaudeRemoteEnvRefusesWhenPickerGone(t *testing.T) {
	menu := newFakeRemoteEnvMenu(digitCommits)
	menu.closed = true
	err := ClaudeRemoteEnv(context.Background(), menu.config(), "1")
	if err == nil || !strings.Contains(err.Error(), "no longer showing") {
		t.Fatalf("error = %v, want the missing picker to surface", err)
	}
	if len(menu.keys) != 0 {
		t.Errorf("no keystroke may be sent when the picker is gone, got %v", menu.keys)
	}
}

// Keystroke and read failures propagate.
func TestClaudeRemoteEnvPropagatesFailures(t *testing.T) {
	menu := newFakeRemoteEnvMenu(digitMovesCaret)
	menu.failKey = "enter"
	if err := ClaudeRemoteEnv(context.Background(), menu.config(), "2"); err == nil ||
		!strings.Contains(err.Error(), "commit") {
		t.Fatalf("error = %v, want the failing commit keystroke to surface", err)
	}

	menu = newFakeRemoteEnvMenu(digitCommits)
	cfg := menu.config()
	cfg.Read = func(context.Context, string, int) (string, error) {
		return "", errors.New("induced read failure")
	}
	if err := ClaudeRemoteEnv(context.Background(), cfg, "1"); err == nil ||
		!strings.Contains(err.Error(), "induced read failure") {
		t.Fatalf("error = %v, want the induced read failure", err)
	}
}
