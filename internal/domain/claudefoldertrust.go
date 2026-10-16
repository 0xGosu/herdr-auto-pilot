package domain

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Claude Code asks, at the first launch in a folder no trusted ancestor
// covers, whether to trust it. Verified live against Claude Code 2.1.301 on
// herdr 0.9.1 (fixtures approval_claude_folder_trust*.txt):
//
//	Accessing workspace:
//
//	/var/tmp/hap-trust-long-0zze
//
//	Quick safety check: Is this a project you created or one you trust? …
//	Claude Code'll be able to read, edit, and execute files here.
//
//	⚠ This folder pre-approves 4 tool permissions in .claude/settings.json:
//	  mcp__gosu, mcp__claude-in-chrome, mcp__ide__getDiagnostics, and Skill
//	⚠ This folder adds 2 directories to the workspace in .claude/settings.json:
//	  /tmp/ and ~/.claude/
//	These will apply without asking. Only proceed if you trust this configuration.
//
//	Security guide
//
//	❯ No, exit
//	  Yes, I trust this folder
//
//	Enter to confirm · Esc to cancel
//
// The ⚠ blocks appear only when the folder's .claude/settings.json widens
// Claude's permissions. Claude does NOT list hooks, env, or .mcp.json servers
// here (verified with a folder carrying all three), and truncates a long
// permission list to "…, and N more".
//
// The rows are UNNUMBERED: the caret moves with the arrow keys and Enter
// commits the row under it, so no digit maps onto this form. The caret starts
// on "No, exit" in 2.1.301 but started on the trust row in 2.1.206, so a reply
// is mapped by LABEL and the caret walked to it — never "Enter, it's on Yes".
// herdr reports the pane BLOCKED while the dialog stands (and `agent start`
// refuses with agent_not_ready).
var (
	// End-of-text anchor, like the remote-environment picker's: a live dialog
	// is the bottom of the capture, so a copy narrated in scrollback can never
	// read as standing. Older builds said "Esc to exit".
	claudeTrustFooterRE = regexp.MustCompile(`^\s*Enter to confirm\s*·\s*Esc to (?:cancel|exit)\s*$`)
	// The question's first words survive any pane width (it wraps after them).
	// The older "Do you trust the files in this folder?" wording is accepted
	// too: the option-row requirement below is what proves the form.
	claudeTrustQuestionRE  = regexp.MustCompile(`^\s*(?:Quick safety check:|Do you trust the files in this folder\?)`)
	claudeTrustWorkspaceRE = regexp.MustCompile(`^\s*Accessing workspace:\s*$`)
	// An option row: the caret or its indent, an optional "N." an older build
	// may have drawn, then the label.
	claudeTrustRowRE     = regexp.MustCompile(`^\s*(❯\s+|\s{2,})(?:\d+[.)]\s+)?(\S.*?)\s*$`)
	claudeTrustWarnRE    = regexp.MustCompile(`^\s*⚠\s*(.*?)\s*$`)
	claudeTrustWarnEndRE = regexp.MustCompile(`^\s*(?:These will apply without asking|Security guide\s*$)`)
	claudeTrustMoreRE    = regexp.MustCompile(`(?i),?\s+and\s+\d+\s+more\s*$`)
)

// PermissionVerbClaudeFolderTrust is the fixed permission verb of Claude's
// folder-trust dialog. The folder and its listed settings are what an operator
// decides on, not part of the key: hap never answers this dialog on its own
// (ReasonFolderTrust), so there is nothing a per-folder signature would buy.
const PermissionVerbClaudeFolderTrust = "trust this folder"

// ClaudeFolderTrustWarning is one ⚠ block: what trusting the folder applies
// without asking.
type ClaudeFolderTrustWarning struct {
	// Heading as Claude words it, without the ⚠ and the trailing colon:
	// "This folder pre-approves 4 tool permissions in .claude/settings.json".
	Heading string
	// Items is the listed entries, rejoined across wrapped lines:
	// "mcp__gosu, mcp__claude-in-chrome, mcp__ide__getDiagnostics, and Skill".
	Items string
	// Truncated reports that Claude cut the list short ("…, and 4 more").
	Truncated bool
}

// ClaudeFolderTrustForm is the parsed live state of Claude's folder-trust
// dialog.
type ClaudeFolderTrustForm struct {
	Region string
	// Folder is the path under "Accessing workspace:", "" when the capture
	// does not reach it (callers fall back to the pane's cwd).
	Folder   string
	Warnings []ClaudeFolderTrustWarning
	// Options are the rows in display order, numbered 1..n by POSITION — the
	// numbers are hap's, not Claude's, and are never pressed as digits.
	Options []NumberedOption
	// Caret is the 0-based row under ❯, -1 when not drawn.
	Caret int
}

// ClaudeFolderTrust reports whether pane shows Claude's live folder-trust
// dialog and parses it. Callers gate on agent type claude.
//
// From the bottom up it requires the footer as the last line, a run of 2-4
// option rows directly above it (one blank between) that includes both an
// exit row and a trust row, and the question above those rows. The exit and
// trust rows together are what make a narrated "Enter to confirm" or a
// different Claude dialog unable to match.
func ClaudeFolderTrust(pane string) (ClaudeFolderTrustForm, bool) {
	lines := trimTrailingBlank(strings.Split(strings.ReplaceAll(pane, "\r", ""), "\n"))
	n := len(lines)
	if n < 4 || !claudeTrustFooterRE.MatchString(lines[n-1]) {
		return ClaudeFolderTrustForm{}, false
	}
	i := n - 2
	for i >= 0 && strings.TrimSpace(lines[i]) == "" {
		i--
	}
	last := i
	// The rows end at the first line that is not one: a blank (as rendered) or
	// the 1-space "Security guide" (as pasted with blanks stripped).
	for i >= 0 && strings.TrimSpace(lines[i]) != "" && claudeTrustRowRE.MatchString(lines[i]) {
		i--
	}
	first := i + 1
	rows := lines[first : last+1]
	if len(rows) < 2 || len(rows) > 4 {
		return ClaudeFolderTrustForm{}, false
	}
	form := ClaudeFolderTrustForm{Caret: -1}
	exitRow, trustRow := false, false
	for k, l := range rows {
		m := claudeTrustRowRE.FindStringSubmatch(l)
		if m == nil {
			return ClaudeFolderTrustForm{}, false
		}
		if strings.HasPrefix(m[1], "❯") {
			if form.Caret >= 0 {
				return ClaudeFolderTrustForm{}, false
			}
			form.Caret = k
		}
		label := m[2]
		folded := FoldMenuText(label)
		exitRow = exitRow || strings.HasPrefix(folded, "no, exit") || folded == "no"
		trustRow = trustRow || strings.Contains(folded, "trust this folder") || strings.HasPrefix(folded, "yes, proceed")
		form.Options = append(form.Options, NumberedOption{Number: strconv.Itoa(k + 1), Label: label})
	}
	if !exitRow || !trustRow {
		return ClaudeFolderTrustForm{}, false
	}
	q := lastLineIndex(lines, first, claudeTrustQuestionRE)
	if q < 0 {
		return ClaudeFolderTrustForm{}, false
	}
	start := q
	// "Accessing workspace:" and the path sit just above the question.
	for k := q - 1; k >= 0 && k >= q-6; k-- {
		if claudeTrustWorkspaceRE.MatchString(lines[k]) {
			start = k
			form.Folder = trustFolderPath(lines[k+1 : q])
			break
		}
	}
	form.Warnings = trustWarnings(lines[q+1 : first])
	form.Region = strings.Join(lines[start:], "\n")
	return form, true
}

// ClaudeFolderTrustStanding reports whether pane shows Claude's folder-trust
// dialog on a claude agent — the check every path that TYPES into a pane asks:
// typed text is ignored there and the Enter that follows commits the row under
// the caret, which is "No, exit" (ending the session) on current builds and
// was "Yes, I trust this folder" on older ones.
func ClaudeFolderTrustStanding(agentType, pane string) bool {
	if !strings.EqualFold(strings.TrimSpace(agentType), "claude") {
		return false
	}
	_, ok := ClaudeFolderTrust(pane)
	return ok
}

// trustFolderPath reads the path printed under "Accessing workspace:". A path
// wider than the pane wraps; the pieces are rejoined with nothing between, as
// a path has no spaces the wrap could have eaten that matter more than the
// ones it inserts.
func trustFolderPath(lines []string) string {
	var parts []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" {
			if len(parts) > 0 {
				break
			}
			continue
		}
		parts = append(parts, t)
	}
	return strings.Join(parts, "")
}

// trustWarnings collects the ⚠ blocks between the question and the rows. A
// block runs to the next ⚠, a blank line, or the closing "These will apply…"
// sentence; its wrapped lines are rejoined with single spaces. Any ⚠ block is
// kept, not just the two kinds seen so far, so a warning a later Claude build
// adds still reaches the escalation.
func trustWarnings(lines []string) []ClaudeFolderTrustWarning {
	var out []ClaudeFolderTrustWarning
	var cur []string
	flush := func() {
		if len(cur) == 0 {
			return
		}
		text := strings.Join(strings.Fields(strings.Join(cur, " ")), " ")
		cur = nil
		w := ClaudeFolderTrustWarning{Heading: text}
		if k := strings.Index(text, ": "); k >= 0 {
			w.Heading, w.Items = text[:k], strings.TrimSpace(text[k+2:])
		} else {
			w.Heading = strings.TrimSuffix(text, ":")
		}
		w.Truncated = claudeTrustMoreRE.MatchString(w.Items)
		out = append(out, w)
	}
	for _, l := range lines {
		if m := claudeTrustWarnRE.FindStringSubmatch(l); m != nil {
			flush()
			cur = []string{m[1]}
			continue
		}
		if cur == nil {
			continue
		}
		if strings.TrimSpace(l) == "" || claudeTrustWarnEndRE.MatchString(l) {
			flush()
			continue
		}
		cur = append(cur, strings.TrimSpace(l))
	}
	flush()
	return out
}

// SameAs reports whether g is the same standing dialog as f — the same folder,
// warnings and rows. The caret is not part of it: walking the caret does not
// change which dialog stands. The folder is compared only when both captures
// show it: the classification read is a consuming delta that can start below
// the "Accessing workspace:" header, and its absence is not a different folder.
func (f ClaudeFolderTrustForm) SameAs(g ClaudeFolderTrustForm) bool {
	if !sameFolder(f.Folder, g.Folder) || len(f.Warnings) != len(g.Warnings) || len(f.Options) != len(g.Options) {
		return false
	}
	for i := range f.Warnings {
		if f.Warnings[i] != g.Warnings[i] {
			return false
		}
	}
	for i := range f.Options {
		if f.Options[i] != g.Options[i] {
			return false
		}
	}
	return true
}

// OptionLabels returns the row labels in display order.
func (f ClaudeFolderTrustForm) OptionLabels() []string {
	return NumberedOptionLabels(f.Options)
}

// Row maps reply — a row label (folded like every menu reply), a unique prefix
// of one, or its 1-based position — onto the 0-based row to walk the caret to.
func (f ClaudeFolderTrustForm) Row(reply string) (int, bool) {
	key, ok := MenuKeystrokeFrom(f.Options, reply)
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(key)
	if err != nil || n < 1 || n > len(f.Options) {
		return 0, false
	}
	return n - 1, true
}

// Summary is the escalation rationale: everything an operator needs to decide
// from the escalation alone, and an honest statement of what it does not show.
// folder is the dialog's own path when it has one, else the caller's fallback
// (the pane's cwd); "" renders as unknown.
func (f ClaudeFolderTrustForm) Summary(folder string) string {
	if f.Folder != "" {
		folder = f.Folder
	}
	if folder == "" {
		folder = "(folder not shown on screen)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Claude asks to trust %s", folder)
	if len(f.Warnings) == 0 {
		b.WriteString("; its .claude/settings.json pre-approves nothing")
	}
	truncated := false
	for _, w := range f.Warnings {
		fmt.Fprintf(&b, "; %s", w.Heading)
		if w.Items != "" {
			fmt.Fprintf(&b, ": %s", w.Items)
		}
		truncated = truncated || w.Truncated
	}
	if truncated {
		b.WriteString(" (Claude truncated the list; read the folder's .claude/settings.json for the rest)")
	}
	b.WriteString(". Trusting applies these without asking; Claude does not list hooks, env or .mcp.json " +
		"servers here. hap never answers this itself — resolve with the row label")
	return b.String()
}
