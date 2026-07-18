package domain

import (
	"strings"
	"testing"
)

const remoteEnvPicker = `● 2 background agents launched (↓ to manage)
▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔
   Select remote environment

   Configure environments at: https://claude.ai/code

   ❯ 1. herdr-auto-pilot (env_01F41H1jxkGrT2zj55CqE4WQ) ✔
     2. myspec-monorepo (env_01CASfztpZp7mYRJPK41sGvK)
     3. Full-access (env_011CUW5BKtc4vkq5q1uSp7MY)
     4. Default (env_011CUKn5Aj1q6ujg5PFvEhTE)

   Enter to select · Esc to cancel
`

func TestClaudeRemoteEnvFormParsesLivePicker(t *testing.T) {
	form, ok := ClaudeRemoteEnvForm(remoteEnvPicker)
	if !ok {
		t.Fatal("live picker must parse")
	}
	if len(form.Options) != 4 {
		t.Fatalf("options = %d (%v), want 4", len(form.Options), form.Options)
	}
	// The default entry's trailing ✔ is UI state, not part of the label.
	if form.Options[0].Label != "herdr-auto-pilot (env_01F41H1jxkGrT2zj55CqE4WQ)" {
		t.Errorf("option 1 label = %q, want ✔ stripped", form.Options[0].Label)
	}
	if form.Options[3] != (NumberedOption{Number: "4", Label: "Default (env_011CUKn5Aj1q6ujg5PFvEhTE)"}) {
		t.Errorf("option 4 = %+v", form.Options[3])
	}
	if form.SelectedOption != "1" {
		t.Errorf("caret = %q, want 1", form.SelectedOption)
	}
	if labels := form.OptionLabels(); len(labels) != 4 || labels[2] != "Full-access (env_011CUW5BKtc4vkq5q1uSp7MY)" {
		t.Errorf("OptionLabels = %v", labels)
	}
}

func TestClaudeRemoteEnvFormRejectsNonLiveShapes(t *testing.T) {
	live := remoteEnvPicker
	cases := []struct {
		name string
		pane string
	}{
		{"empty", ""},
		{"missing title", strings.Replace(live, "Select remote environment", "Pick something", 1)},
		{"missing footer", strings.Replace(live, "Enter to select · Esc to cancel", "", 1)},
		// A stale copy in scrollback with narration below the footer is not a
		// standing modal (footer is end-anchored).
		{"narration after footer", live + "\n● Environment selected, launching agent…\n"},
		// Footer occurring before the title (e.g. an older picker's footer in
		// scrollback above a narrated title) is not a live form either.
		{"footer before title", "   Enter to select · Esc to cancel\n   Select remote environment\n"},
		{"too few options", "   Select remote environment\n\n   ❯ 1. only-env (env_01AAAAAAAAAAAAAAAAAAAAAAAA)\n\n   Enter to select · Esc to cancel\n"},
	}
	for _, tc := range cases {
		if _, ok := ClaudeRemoteEnvForm(tc.pane); ok {
			t.Errorf("%s: must not parse as a live picker", tc.name)
		}
	}
}

func TestClaudeRemoteEnvFormScopesOptionsToRegion(t *testing.T) {
	// A numbered list in scrollback above the picker must not leak into the
	// option set — options come from the title→footer region only.
	pane := "Here is what I changed:\n1. Fixed the parser\n2. Added a test\n\n" + remoteEnvPicker
	form, ok := ClaudeRemoteEnvForm(pane)
	if !ok {
		t.Fatal("picker with scrollback above must still parse")
	}
	if len(form.Options) != 4 {
		t.Fatalf("options = %d (%v), want the 4 picker entries only", len(form.Options), form.Options)
	}
	if strings.Contains(strings.Join(form.OptionLabels(), "|"), "Fixed the parser") {
		t.Errorf("scrollback list leaked into options: %v", form.Options)
	}
}

func TestClaudeRemoteEnvFormLastTitleWins(t *testing.T) {
	// A consuming "recent" read can carry an earlier render above the live
	// one; the last title occurrence is the live form.
	stale := strings.Replace(remoteEnvPicker, "Enter to select · Esc to cancel", "", 1)
	form, ok := ClaudeRemoteEnvForm(stale + "\n" + remoteEnvPicker)
	if !ok {
		t.Fatal("repeated render must parse")
	}
	if len(form.Options) != 4 {
		t.Fatalf("options = %d, want 4 (from the last render only)", len(form.Options))
	}
}

func TestClaudeRemoteEnvFormTwoCompleteRendersParsesLive(t *testing.T) {
	// A capture holding TWO complete renders (an earlier one WITH its footer,
	// then the live one) must pair the live title with the live footer — a
	// first-footer pairing would reject the live frame and reintroduce the
	// silent-block failure this detector exists to fix. The earlier render's
	// options differ so the assertion proves which render was parsed.
	earlier := strings.ReplaceAll(remoteEnvPicker, "herdr-auto-pilot", "old-project")
	form, ok := ClaudeRemoteEnvForm(earlier + "\n● narration between renders\n\n" + remoteEnvPicker)
	if !ok {
		t.Fatal("capture with two complete renders must parse as the live form")
	}
	if len(form.Options) != 4 || !strings.HasPrefix(form.Options[0].Label, "herdr-auto-pilot") {
		t.Fatalf("parsed the wrong render: %+v", form.Options)
	}
}

func TestClaudeRemoteEnvApprovalSignaturePassesOverMaskGuard(t *testing.T) {
	// The enriched approval situation must produce a stable, unmasked salient
	// so the decision core never dismisses the picker as over_masked (the
	// original failure: classified idle, raw trailing-window salient tripped
	// the guard).
	form, ok := ClaudeRemoteEnvForm(remoteEnvPicker)
	if !ok {
		t.Fatal("fixture no longer parses as a remote-env picker")
	}
	s := Situation{
		AgentType:      "claude",
		Type:           SituationApproval,
		Content:        remoteEnvPicker,
		PermissionVerb: PermissionVerbSelectRemoteEnv,
		Options:        form.OptionLabels(),
	}
	sig := ComputeSignature(s)
	if sig.Verdict != GuardOK {
		t.Fatalf("verdict = %v, want %v (salient %q)", sig.Verdict, GuardOK, sig.Salient)
	}
	// The env labels are the learned action, not the key: even with Options
	// populated the salient stays verb-only (the issue #155 option folding
	// exempts this picker).
	if sig.Salient != "permission:select remote environment" {
		t.Errorf("salient = %q", sig.Salient)
	}
}
