package domain

import (
	"fmt"
	"strings"
)

// FolderTrustPrompt is an agent's "do you trust this folder?" prompt — Claude
// Code's folder-trust dialog or agy's trust prompt. Trusting lets the agent
// read, edit and execute files there (and, for Claude, applies the folder's
// .claude/settings.json without asking), so it is the operator's call alone:
// domain.Decide vetoes it as ReasonFolderTrust, and every gate that refuses it
// asks FolderTrustSituation or FolderTrustStanding — so a new agent's prompt is
// added here, once.
type FolderTrustPrompt struct {
	claude *ClaudeFolderTrustForm
	agy    *AgyTrustForm
}

// FolderTrustSituation reports whether s is a folder-trust prompt: an approval
// whose capture shows one standing, on the agent that draws it.
func FolderTrustSituation(s Situation) (FolderTrustPrompt, bool) {
	if s.Type != SituationApproval {
		return FolderTrustPrompt{}, false
	}
	return folderTrustOn(s.AgentType, s.Content)
}

// FolderTrustStanding reports whether pane shows a folder-trust prompt on an
// agent of agentType — the check for paths that must not answer it whatever
// the situation they were decided on.
func FolderTrustStanding(agentType, pane string) bool {
	_, ok := folderTrustOn(agentType, pane)
	return ok
}

func folderTrustOn(agentType, pane string) (FolderTrustPrompt, bool) {
	switch {
	case strings.EqualFold(strings.TrimSpace(agentType), "claude"):
		if f, ok := ClaudeFolderTrust(pane); ok {
			return FolderTrustPrompt{claude: &f}, true
		}
	case IsAgy(agentType):
		if f, ok := ParseAgyTrust(pane); ok {
			return FolderTrustPrompt{agy: &f}, true
		}
	}
	return FolderTrustPrompt{}, false
}

// Folder is the folder the prompt names on screen, "" when the capture does not
// reach it (callers fall back to the pane's cwd).
func (p FolderTrustPrompt) Folder() string {
	switch {
	case p.claude != nil:
		return p.claude.Folder
	case p.agy != nil:
		return p.agy.Folder
	}
	return ""
}

// Summary is the escalation rationale: what the operator decides on, from the
// escalation alone. folder is the fallback when the prompt does not name one.
func (p FolderTrustPrompt) Summary(folder string) string {
	if p.claude != nil {
		return p.claude.Summary(folder)
	}
	if p.agy == nil {
		return ""
	}
	if p.agy.Folder != "" {
		folder = p.agy.Folder
	}
	if folder == "" {
		folder = "(folder not shown on screen)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "agy asks to trust %s", folder)
	if p.agy.Note != "" {
		fmt.Fprintf(&b, ": %q", p.agy.Note)
	}
	b.WriteString(". Trusting lets agy read, edit and execute files there. hap never answers this itself — " +
		"resolve with the row label")
	return b.String()
}
