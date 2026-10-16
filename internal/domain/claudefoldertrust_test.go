package domain

import (
	"os"
	"strings"
	"testing"
)

func readTrustFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("../classify/testdata/transcripts/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// trustPasted is the dialog as the operator pasted it from audit
// #236762440454766592: blank lines stripped and no "Accessing workspace:"
// header. The parser must still read it, and fall back to the pane's cwd for
// the folder.
const trustPasted = ` Quick safety check: Is this a project you created or one you trust? (Like your own code, a well-known open source project, or work from your team). If not, take a moment to review what's in this folder first.
 Claude Code'll be able to read, edit, and execute files here.
 ⚠ This folder pre-approves 4 tool permissions in .claude/settings.json:
   mcp__gosu, mcp__claude-in-chrome, mcp__ide__getDiagnostics, and Skill
 ⚠ This folder adds 2 directories to the workspace in .claude/settings.json:
   /tmp/ and ~/.claude/
 These will apply without asking. Only proceed if you trust this configuration.
 Security guide
 ❯ No, exit
   Yes, I trust this folder
 Enter to confirm · Esc to cancel
`

func TestClaudeFolderTrustParsesTheShortDialog(t *testing.T) {
	f, ok := ClaudeFolderTrust(readTrustFixture(t, "approval_claude_folder_trust.txt"))
	if !ok {
		t.Fatal("short folder-trust dialog not recognised")
	}
	if f.Folder != "/var/tmp/hap-trust-short-e6S7" {
		t.Errorf("folder = %q", f.Folder)
	}
	if len(f.Warnings) != 0 {
		t.Errorf("warnings = %+v, want none", f.Warnings)
	}
	if got := strings.Join(f.OptionLabels(), "|"); got != "No, exit|Yes, I trust this folder" {
		t.Errorf("options = %q", got)
	}
	if f.Caret != 0 {
		t.Errorf("caret = %d, want 0 (No, exit)", f.Caret)
	}
	if !strings.HasPrefix(f.Region, " Accessing workspace:") {
		t.Errorf("region does not start at the workspace header: %q", f.Region)
	}
	if s := f.Summary(""); !strings.Contains(s, "/var/tmp/hap-trust-short-e6S7") ||
		!strings.Contains(s, "pre-approves nothing") {
		t.Errorf("summary = %q", s)
	}
}

func TestClaudeFolderTrustParsesTheSettingsWarnings(t *testing.T) {
	f, ok := ClaudeFolderTrust(readTrustFixture(t, "approval_claude_folder_trust_settings.txt"))
	if !ok {
		t.Fatal("settings folder-trust dialog not recognised")
	}
	want := []ClaudeFolderTrustWarning{
		{Heading: "This folder pre-approves 4 tool permissions in .claude/settings.json",
			Items: "mcp__gosu, mcp__claude-in-chrome, mcp__ide__getDiagnostics, and Skill"},
		{Heading: "This folder adds 2 directories to the workspace in .claude/settings.json",
			Items: "/tmp/ and ~/.claude/"},
	}
	if len(f.Warnings) != len(want) {
		t.Fatalf("warnings = %+v", f.Warnings)
	}
	for i := range want {
		if f.Warnings[i] != want[i] {
			t.Errorf("warning %d = %+v, want %+v", i, f.Warnings[i], want[i])
		}
	}
	s := f.Summary("/ignored/cwd")
	for _, part := range []string{"/var/tmp/hap-trust-long-0zze", "mcp__ide__getDiagnostics, and Skill",
		"/tmp/ and ~/.claude/", "hooks, env or .mcp.json"} {
		if !strings.Contains(s, part) {
			t.Errorf("summary lacks %q: %s", part, s)
		}
	}
	if strings.Contains(s, "/ignored/cwd") || strings.Contains(s, "truncated") {
		t.Errorf("summary = %s", s)
	}
}

// A narrow pane wraps the headings and lists onto 1-space continuation lines,
// and Claude truncates a long permission list. Both must survive into the
// warning, and the summary must say the list is incomplete.
func TestClaudeFolderTrustRejoinsWrappedAndTruncatedWarnings(t *testing.T) {
	f, ok := ClaudeFolderTrust(readTrustFixture(t, "approval_claude_folder_trust_narrow.txt"))
	if !ok {
		t.Fatal("narrow folder-trust dialog not recognised")
	}
	if len(f.Warnings) != 2 {
		t.Fatalf("warnings = %+v", f.Warnings)
	}
	perms := f.Warnings[0]
	if perms.Heading != "This folder pre-approves 12 tool permissions in .claude/settings.json" ||
		!strings.HasPrefix(perms.Items, "mcp__gosu, mcp__claude-in-chrome, mcp__ide__getDiagnostics, mcp__github,") ||
		!strings.HasSuffix(perms.Items, "Edit(**), and 4 more") || !perms.Truncated {
		t.Errorf("permissions warning = %+v", perms)
	}
	if dirs := f.Warnings[1]; dirs.Items != "/tmp/, ~/.claude/, /etc, and /var/log" || dirs.Truncated {
		t.Errorf("directories warning = %+v", dirs)
	}
	if s := f.Summary(""); !strings.Contains(s, "Claude truncated the list") {
		t.Errorf("summary does not flag the truncation: %s", s)
	}
}

func TestClaudeFolderTrustReadsThePastedDialogAndFallsBackToTheCwd(t *testing.T) {
	f, ok := ClaudeFolderTrust(trustPasted)
	if !ok {
		t.Fatal("pasted dialog not recognised")
	}
	if f.Folder != "" || len(f.Warnings) != 2 || f.Caret != 0 {
		t.Errorf("form = %+v", f)
	}
	if s := f.Summary("/workspaces/vtcedu-monorepo"); !strings.Contains(s, "trust /workspaces/vtcedu-monorepo") {
		t.Errorf("summary = %s", s)
	}
	if s := f.Summary(""); !strings.Contains(s, "folder not shown") {
		t.Errorf("summary = %s", s)
	}
}

func TestClaudeFolderTrustRefusesLookalikes(t *testing.T) {
	pane := readTrustFixture(t, "approval_claude_folder_trust.txt")
	cases := map[string]string{
		// Anything drawn below the footer: the dialog is not the live bottom.
		"text below footer": pane + "\n❯ continue\n",
		// A different Claude dialog with the same footer.
		"no trust row": strings.Replace(pane, "Yes, I trust this folder", "Yes, continue", 1),
		"no exit row":  strings.Replace(pane, "No, exit", "Maybe later", 1),
		"no question":  strings.Replace(pane, "Quick safety check:", "Heads up:", 1),
		"two carets":   strings.Replace(pane, "   Yes, I trust", " ❯ Yes, I trust", 1),
		"no footer":    strings.Replace(pane, "Enter to confirm · Esc to cancel", "", 1),
	}
	for name, p := range cases {
		if _, ok := ClaudeFolderTrust(p); ok {
			t.Errorf("%s: recognised as the folder-trust dialog", name)
		}
	}
}

// The caret is part of the screen, not of the dialog's identity, and a reply
// maps by label (or unique prefix) onto a row index — never onto a digit
// Claude would see.
func TestClaudeFolderTrustRowsAndIdentity(t *testing.T) {
	pane := readTrustFixture(t, "approval_claude_folder_trust_settings.txt")
	f, _ := ClaudeFolderTrust(pane)
	moved, ok := ClaudeFolderTrust(strings.NewReplacer(" ❯ No, exit", "   No, exit",
		"   Yes, I trust this folder", " ❯ Yes, I trust this folder").Replace(pane))
	if !ok || moved.Caret != 1 {
		t.Fatalf("caret on the trust row not parsed: %+v", moved)
	}
	if !f.SameAs(moved) {
		t.Error("moving the caret changed the dialog's identity")
	}
	other, _ := ClaudeFolderTrust(strings.Replace(pane, "and Skill", "and Bash", 1))
	if f.SameAs(other) {
		t.Error("a different permission list compared equal")
	}
	for reply, want := range map[string]int{
		"Yes, I trust this folder": 1, "yes, i trust this folder": 1, "Yes": 1,
		"No, exit": 0, "No": 0, "2": 1,
	} {
		if got, ok := f.Row(reply); !ok || got != want {
			t.Errorf("Row(%q) = %d, %v; want %d", reply, got, ok, want)
		}
	}
	for _, reply := range []string{"Yes, proceed", "trust", "3", ""} {
		if _, ok := f.Row(reply); ok {
			t.Errorf("Row(%q) mapped", reply)
		}
	}
}
