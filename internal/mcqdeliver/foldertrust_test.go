package mcqdeliver

import (
	"context"
	"os"
	"strings"
	"testing"
)

// fakeFolderTrust simulates Claude's folder-trust dialog by redrawing the real
// captured screen with the caret moved, so the production parser reads it.
// Digits do nothing (the dialog has none); up/down move the caret; Enter
// commits the row under it.
type fakeFolderTrust struct {
	screen   string // the captured fixture, caret on row 0
	rows     []string
	caret    int
	selected string
	keys     []string
	// frozen models a caret that ignores the arrows.
	frozen bool
}

func newFakeFolderTrust(t *testing.T, fixture string) *fakeFolderTrust {
	t.Helper()
	b, err := os.ReadFile("../classify/testdata/transcripts/" + fixture)
	if err != nil {
		t.Fatal(err)
	}
	return &fakeFolderTrust{screen: string(b), rows: []string{"No, exit", "Yes, I trust this folder"}}
}

func (f *fakeFolderTrust) SendKey(_ context.Context, _, key string) error {
	f.keys = append(f.keys, key)
	if f.selected != "" {
		return nil
	}
	switch key {
	case "down":
		if !f.frozen && f.caret < len(f.rows)-1 {
			f.caret++
		}
	case "up":
		if !f.frozen && f.caret > 0 {
			f.caret--
		}
	case "enter":
		f.selected = f.rows[f.caret]
	}
	return nil
}

func (f *fakeFolderTrust) Read(_ context.Context, _ string, _ int) (string, error) {
	if f.selected != "" {
		return "❯ \n  ? for shortcuts\n", nil
	}
	screen := f.screen
	if f.caret == 1 {
		screen = strings.NewReplacer(" ❯ No, exit", "   No, exit",
			"   Yes, I trust this folder", " ❯ Yes, I trust this folder").Replace(screen)
	}
	return screen, nil
}

func (f *fakeFolderTrust) cfg() Config {
	return Config{Keys: f, Read: f.Read, PaneID: "p1", ReadLines: 60}
}

func TestClaudeFolderTrustWalksTheCaretByLabel(t *testing.T) {
	f := newFakeFolderTrust(t, "approval_claude_folder_trust_settings.txt")
	if err := ClaudeFolderTrust(context.Background(), f.cfg(), f.screen, "Yes, I trust this folder"); err != nil {
		t.Fatal(err)
	}
	if f.selected != "Yes, I trust this folder" {
		t.Errorf("committed %q", f.selected)
	}
	if got := strings.Join(f.keys, " "); got != "down enter" {
		t.Errorf("keys = %q, want \"down enter\"", got)
	}
}

func TestClaudeFolderTrustAnswersTheRowUnderTheCaretWithEnterAlone(t *testing.T) {
	f := newFakeFolderTrust(t, "approval_claude_folder_trust.txt")
	if err := ClaudeFolderTrust(context.Background(), f.cfg(), "", "No, exit"); err != nil {
		t.Fatal(err)
	}
	if f.selected != "No, exit" || strings.Join(f.keys, " ") != "enter" {
		t.Errorf("selected %q with keys %q", f.selected, f.keys)
	}
}

// An older build drew the caret on the trust row; "No" must walk UP to it.
func TestClaudeFolderTrustWalksUpWhenTheCaretStartsOnTrust(t *testing.T) {
	f := newFakeFolderTrust(t, "approval_claude_folder_trust.txt")
	f.caret = 1
	if err := ClaudeFolderTrust(context.Background(), f.cfg(), "", "No"); err != nil {
		t.Fatal(err)
	}
	if f.selected != "No, exit" || strings.Join(f.keys, " ") != "up enter" {
		t.Errorf("selected %q with keys %q", f.selected, f.keys)
	}
}

func TestClaudeFolderTrustFailsClosed(t *testing.T) {
	other := newFakeFolderTrust(t, "approval_claude_folder_trust_settings.txt").screen
	cases := map[string]struct {
		setup  func(*fakeFolderTrust)
		reply  string
		decide func(*fakeFolderTrust) string
	}{
		"unmatched label": {reply: "Yes, proceed"},
		// Enter with the caret short of the chosen row would commit the wrong one.
		"caret does not move": {reply: "Yes, I trust this folder", setup: func(f *fakeFolderTrust) { f.frozen = true }},
		// The operator decided on a folder with no settings; the live dialog
		// lists pre-approved permissions.
		"different dialog": {reply: "Yes, I trust this folder",
			decide: func(f *fakeFolderTrust) string { return f.screen },
			setup:  func(f *fakeFolderTrust) { f.screen = other }},
		"dialog gone": {reply: "No, exit", setup: func(f *fakeFolderTrust) { f.selected = "x" }},
	}
	for name, tc := range cases {
		f := newFakeFolderTrust(t, "approval_claude_folder_trust.txt")
		decided := ""
		if tc.decide != nil {
			decided = tc.decide(f)
		}
		if tc.setup != nil {
			tc.setup(f)
		}
		if err := ClaudeFolderTrust(context.Background(), f.cfg(), decided, tc.reply); err == nil {
			t.Errorf("%s: delivered", name)
		}
		for _, k := range f.keys {
			if k == "enter" {
				t.Errorf("%s: pressed enter (%q)", name, f.keys)
			}
		}
	}
}

// No digit ever reaches the dialog, whatever the reply looks like.
func TestClaudeFolderTrustNeverPressesADigit(t *testing.T) {
	f := newFakeFolderTrust(t, "approval_claude_folder_trust.txt")
	if err := ClaudeFolderTrust(context.Background(), f.cfg(), "", "2"); err != nil {
		t.Fatal(err)
	}
	if f.selected != "Yes, I trust this folder" || strings.Join(f.keys, " ") != "down enter" {
		t.Errorf("selected %q with keys %q", f.selected, f.keys)
	}
}
