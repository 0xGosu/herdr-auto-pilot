package domain

import (
	"os"
	"strings"
	"testing"
)

func readAgyTrustFixture(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../classify/testdata/transcripts/approval_agy_trust_folder.txt")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAgyTrustPromptNamesItsFolderAndNote(t *testing.T) {
	f, ok := ParseAgyTrust(readAgyTrustFixture(t))
	if !ok {
		t.Fatal("agy trust prompt not parsed")
	}
	if f.Folder != "/tmp/agyscratch.H2CE" {
		t.Errorf("folder = %q", f.Folder)
	}
	if f.Note != "Antigravity CLI requires permission to read, edit, and execute files here." {
		t.Errorf("note = %q", f.Note)
	}
}

// Both agents' prompts are one FolderTrustPrompt, scoped to the agent that
// draws it, and the summary names the folder (or the cwd fallback).
func TestFolderTrustSituationCoversClaudeAndAgy(t *testing.T) {
	agyPane := readAgyTrustFixture(t)
	p, ok := FolderTrustSituation(Situation{Type: SituationApproval, AgentType: AgentTypeAgy, Content: agyPane})
	if !ok {
		t.Fatal("agy trust prompt is not a folder-trust situation")
	}
	if s := p.Summary("/ignored"); !strings.Contains(s, "agy asks to trust /tmp/agyscratch.H2CE") ||
		!strings.Contains(s, "read, edit, and execute files here") {
		t.Errorf("summary = %q", s)
	}
	noHeader := strings.Replace(agyPane, "Accessing workspace:", "", 1)
	p, _ = FolderTrustSituation(Situation{Type: SituationApproval, AgentType: AgentTypeAgy, Content: noHeader})
	if p.Folder() != "" || !strings.Contains(p.Summary("/from/cwd"), "trust /from/cwd") {
		t.Errorf("cwd fallback: folder %q summary %q", p.Folder(), p.Summary("/from/cwd"))
	}

	claudePane, err := os.ReadFile("../classify/testdata/transcripts/approval_claude_folder_trust.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := FolderTrustSituation(Situation{Type: SituationApproval, AgentType: "claude", Content: string(claudePane)}); !ok {
		t.Error("Claude's dialog is not a folder-trust situation")
	}
	// Scoped: each prompt only on its own agent, and only as an approval.
	for _, c := range []struct {
		agent, pane string
		typ         SituationType
	}{
		{"claude", agyPane, SituationApproval},
		{AgentTypeAgy, string(claudePane), SituationApproval},
		{"codex", agyPane, SituationApproval},
		{AgentTypeAgy, agyPane, SituationIdle},
	} {
		if _, ok := FolderTrustSituation(Situation{Type: c.typ, AgentType: c.agent, Content: c.pane}); ok {
			t.Errorf("%s/%s recognised as a folder-trust situation", c.agent, c.typ)
		}
	}
	if !FolderTrustStanding(AgentTypeAgy, agyPane) || FolderTrustStanding("codex", agyPane) {
		t.Error("FolderTrustStanding scoping")
	}
}
