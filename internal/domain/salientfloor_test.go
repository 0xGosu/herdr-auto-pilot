package domain

import (
	"strings"
	"testing"
)

// TestSalientContentStripsClaudeChromeBeforeWindow pins the ORDER of the two
// operations: the chrome is redacted first, so the pane-tail window spends its
// whole budget on what the agent said. Before the strip, the footer block alone
// filled a narrow window and both panes hashed identically.
func TestSalientContentStripsClaudeChromeBeforeWindow(t *testing.T) {
	pane := func(msg string) string {
		return "● " + msg + "\n\n" + claudeFooter
	}
	const window = 60 // narrower than the footer block on its own
	a := ComputeSignatureN(sit(SituationIdle, "claude", pane("the build finished successfully")), window)
	b := ComputeSignatureN(sit(SituationIdle, "claude", pane("the build failed with three errors")), window)

	if a.Verdict != GuardOK || b.Verdict != GuardOK {
		t.Fatalf("both panes carry real content: %v / %v", a.Verdict, b.Verdict)
	}
	for _, chrome := range []string{"❯", "workspace", "INSERT", "───"} {
		if strings.Contains(a.Salient, chrome) {
			t.Errorf("chrome %q survived into the salient: %q", chrome, a.Salient)
		}
	}
	if a.Raw == b.Raw {
		t.Errorf("chrome must not crowd the window: both panes hashed to %q (salient %q)", a.Raw, a.Salient)
	}
}

// TestSalientContentChromeStripIsClaudeOnly holds StripClaudeChrome to its
// stated contract. "❯" and the block glyphs carry no special meaning for other
// agents, so stripping them there would delete real content.
func TestSalientContentChromeStripIsClaudeOnly(t *testing.T) {
	pane := "● The refactor is complete and every test passes now.\n\n" + claudeFooter
	claude := ComputeSignature(sit(SituationIdle, "claude", pane))
	if strings.Contains(claude.Salient, "workspace") {
		t.Fatalf("premise broken: claude salient still carries chrome: %q", claude.Salient)
	}
	for _, agent := range []string{"codex", "agy", ""} {
		other := ComputeSignature(sit(SituationIdle, agent, pane))
		if !strings.Contains(other.Salient, "workspace") {
			t.Errorf("agent %q must not get the claude chrome strip; salient = %q", agent, other.Salient)
		}
	}
}

// TestSalientContentChromeOnlyPaneIsOverMasked: a pane that is nothing but
// chrome now strips to (almost) nothing and trips the over-masking floor, so it
// escalates instead of minting the near-empty rule that used to answer every
// unrelated screen at cosine 0.91.
func TestSalientContentChromeOnlyPaneIsOverMasked(t *testing.T) {
	got := ComputeSignature(sit(SituationIdle, "claude", claudeBanner+"\n\n"+claudeFooter))
	if got.Verdict != GuardOverMasked {
		t.Errorf("chrome-only pane verdict = %v, want %v (salient %q)",
			got.Verdict, GuardOverMasked, got.Salient)
	}
	if got.Signature != "" {
		t.Errorf("an over-masked situation must mint no key, got %q", got.Signature)
	}
}

// TestSalientContentStructuredBranchesIgnoreChrome: the structured salients are
// distilled identities that never contained chrome, so the claude branch must
// not perturb them.
func TestSalientContentStructuredBranchesIgnoreChrome(t *testing.T) {
	content := "Do you want to proceed?\n" + claudeFooter
	approval := ComputeSignature(Situation{Type: SituationApproval, AgentType: "claude",
		PermissionVerb: "run shell command", Options: []string{"Yes", "No"}, Content: content})
	if !strings.HasPrefix(approval.Salient, "permission:") {
		t.Errorf("approval salient should stay structured, got %q", approval.Salient)
	}
	choice := ComputeSignature(Situation{Type: SituationChoice, AgentType: "claude",
		Options: []string{"Apple", "Banana"}, Content: content})
	if !strings.HasPrefix(choice.Salient, "options:") {
		t.Errorf("choice salient should stay structured, got %q", choice.Salient)
	}
}

func TestEmbeddableSalient(t *testing.T) {
	tests := []struct {
		name     string
		chars    int
		minChars int
		want     bool
	}{
		{"one below the floor", 99, 100, false},
		{"exactly at the floor", 100, 100, true},
		{"one above the floor", 101, 100, true},
		{"empty salient", 0, 100, false},
		{"zero minChars uses the default, below", DefaultMinSalientChars - 1, 0, false},
		{"zero minChars uses the default, at", DefaultMinSalientChars, 0, true},
		{"negative minChars uses the default", DefaultMinSalientChars, -5, true},
		{"custom higher floor", 150, 200, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := EmbeddableSalient(strings.Repeat("a", tc.chars), tc.minChars); got != tc.want {
				t.Errorf("EmbeddableSalient(%d chars, min %d) = %v, want %v",
					tc.chars, tc.minChars, got, tc.want)
			}
		})
	}
}

// TestEmbeddableSalientExemptsStructuredSalients is the invariant that keeps
// the floor from switching semantic matching off for the situations it exists
// to serve. Structured salients are short BY CONSTRUCTION — the real ones below
// are all under 50 characters — so a length floor applied to them would end
// cosine paraphrase matching for every approval, choice and error rule.
func TestEmbeddableSalientExemptsStructuredSalients(t *testing.T) {
	structured := []string{
		"permission:proceed | options:no;yes",
		"permission:run shell command | options:",
		"permission:select remote environment",
		"options:apple;banana",
		"error:usage_limit",
	}
	for _, s := range structured {
		if n := len([]rune(s)); n >= DefaultMinSalientChars {
			t.Fatalf("premise broken: %q is %d chars, expected below the floor", s, n)
		}
		if !StructuredSalient(s) {
			t.Fatalf("premise broken: %q should read as structured", s)
		}
		if !EmbeddableSalient(s, DefaultMinSalientChars) {
			t.Errorf("structured salient %q must stay embeddable at any length", s)
		}
		// Even an absurd floor must not reach them.
		if !EmbeddableSalient(s, 10_000) {
			t.Errorf("structured salient %q must be exempt from any floor", s)
		}
	}
	// A pane-tail salient of the same length is NOT exempt — that is the case
	// the floor is for.
	if EmbeddableSalient("waiting for input", DefaultMinSalientChars) {
		t.Error("a short pane-tail salient must be below the floor")
	}
}

// TestEmbeddableSalientCountsRunes: the floor is a CHARACTER count, so a
// multibyte salient must not be measured by its byte length — CJK content would
// otherwise clear a 100-char floor with 34 characters.
func TestEmbeddableSalientCountsRunes(t *testing.T) {
	salient := strings.Repeat("同", 50) // 50 runes, 150 bytes
	if EmbeddableSalient(salient, 100) {
		t.Errorf("50 runes must not clear a 100-char floor (byte length is 150)")
	}
	if !EmbeddableSalient(strings.Repeat("同", 100), 100) {
		t.Errorf("100 runes must clear a 100-char floor")
	}
}

// TestSalientContentSparseIdlePaneCrossesTheOverMaskFloor pins a real
// consequence of redacting chrome, so it is a reviewed decision rather than a
// surprise: a Claude idle pane whose ONLY content is a couple of words now falls
// under the over-masking floor (overMaskMinContent, 12 word characters) and
// escalates as unidentifiable.
//
// Before the strip such a pane cleared the floor — but only on the strength of
// the banner, status bar and mode line, i.e. it was being keyed on furniture
// identical to every other pane. That is the bug, so padding the floor with
// chrome is not the fix. The boundary is narrow: one ordinary sentence of agent
// output clears it comfortably.
//
// Consequence worth knowing: domain.Decide escalates an over-masked situation
// before it reaches the idle task hand-out, so an agent parked on a near-empty
// screen escalates rather than being handed its next declared task. That
// ordering is pre-existing and deliberately not changed here.
func TestSalientContentSparseIdlePaneCrossesTheOverMaskFloor(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    GuardVerdict
	}{
		{"two words only", "● Done.", GuardOverMasked},
		{"one ordinary sentence clears it", "● Task is complete. Anything else?", GuardOK},
		{"typical idle output clears it", "● All tests pass. The refactor is complete and committed.", GuardOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ComputeSignature(sit(SituationIdle, "claude", tc.content+"\n\n"+claudeFooter))
			if got.Verdict != tc.want {
				t.Errorf("verdict = %v, want %v (salient %q)", got.Verdict, tc.want, got.Salient)
			}
		})
	}
}
