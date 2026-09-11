package domain

import (
	"strings"
	"testing"
)

// The fixture-driven half of agy's coverage lives in internal/classify (it
// runs every recorded screen through the classifier). These cases pin the
// parsers' edges with inline screens: the shapes the corpus does not happen to
// contain but a live pane will, and above all the NEGATIVE cases — a form that
// is no longer the last thing on screen must never read as live.

const agyComposer = `────────────────────────────────────────
>
────────────────────────────────────────
? for shortcuts                                         Gemini 3.6 Flash · low`

func TestCanonicalAgentType(t *testing.T) {
	for in, want := range map[string]string{
		"agy": "agy", "AGY": "agy", " agy ": "agy", "antigravity": "agy", "Antigravity-CLI": "agy",
		"claude": "claude", "codex": "codex", "": "", "unknown": "unknown", "gemini": "gemini",
	} {
		if got := CanonicalAgentType(in); got != want {
			t.Errorf("CanonicalAgentType(%q) = %q, want %q", in, got, want)
		}
	}
	if !IsAgy("antigravity") || IsAgy("claude") || IsAgy("") {
		t.Error("IsAgy must accept exactly agy's aliases")
	}
}

func TestAgyStatusBarLine(t *testing.T) {
	for line, want := range map[string]bool{
		"? for shortcuts": true,
		"? for shortcuts                                     Gemini 3.6 Flash · low":      true,
		"esc to cancel                                  plan · Gemini 3.6 Flash · low":    true,
		"                                          accept-edits · Gemini 3.6 Flash · low": true,
		"                                                   Claude Sonnet 4.6 (Thinking)": true,
		"Gemini 3.6 Flash · low":                          false, // no terminal-width padding: prose
		"                                          Done.": false, // a right-indented sentence
		"                              All tests passed.": false,
		"esc to cancel the build, run make again":         false,
		"? for shortcuts, see the help overlay":           false,
		"  The directory is empty, containing nothing.":   false,
		"": false,
	} {
		if got := agyStatusBarLine(line); got != want {
			t.Errorf("agyStatusBarLine(%q) = %v, want %v", line, got, want)
		}
	}
}

// An approval whose option labels wrapped to column 0 must parse to the same
// labels as the wide render: the salient, and so the learned rule, may not
// depend on the pane's width.
func TestParseAgyApprovalRejoinsColumnZeroWraps(t *testing.T) {
	wide := "Requesting permission for:\n   rm -rf /tmp/x/hello.txt\n\nRun this command?\n" +
		"> 1. Yes, run command\n" +
		"  2. Yes, and always allow in this conversation for commands that start with 'rm -rf /tmp/x/hello.txt'\n" +
		"  3. No, cancel\n\n  ↑/↓ Navigate · tab Amend · ctrl+g edit/expand command\n" +
		"esc to cancel                                         Gemini 3.6 Flash · low\n"
	narrow := "Requesting permission for:\n   rm -rf /tmp/x/hello.txt\n\nRun this command?\n" +
		"> 1. Yes, run command\n" +
		"  2. Yes, and always allow in this conversation for commands that start with 'rm -rf\n" +
		"/tmp/x/hello.txt'\n" +
		"  3. No, cancel\n\n  ↑/↓ Navigate · tab Amend · ctrl+g edit/expand command\n" +
		"esc to cancel                                         Gemini 3.6 Flash · low\n"
	a, ok1 := ParseAgyApproval(wide)
	b, ok2 := ParseAgyApproval(narrow)
	if !ok1 || !ok2 {
		t.Fatalf("both renders must parse: wide=%v narrow=%v", ok1, ok2)
	}
	if strings.Join(NumberedOptionLabels(a.Options), "|") != strings.Join(NumberedOptionLabels(b.Options), "|") {
		t.Errorf("labels differ by width:\n wide   %q\n narrow %q", NumberedOptionLabels(a.Options), NumberedOptionLabels(b.Options))
	}
	if a.Verb != "run" || a.Command != "rm -rf /tmp/x/hello.txt" {
		t.Errorf("verb/command = %q/%q", a.Verb, a.Command)
	}
}

func TestParseAgyApprovalRefusesWhatIsNotLive(t *testing.T) {
	form := "Requesting permission for:\n   ls\n\nRun this command?\n> 1. Yes, run command\n  2. No, cancel\n\n" +
		"  ↑/↓ Navigate · tab Amend · ctrl+g edit/expand command\n"
	for name, pane := range map[string]string{
		// Answered, and agy moved on: the form is scrollback now.
		"a newer turn follows the form": form + "  ⎿  User declined the tool call\n" + agyComposer,
		// The question without its "Requesting permission for:" anchor.
		"no command anchor": "Run this command?\n> 1. Yes, run command\n  2. No, cancel\n\n  ↑/↓ Navigate · tab Amend\n",
		// Prose between the question and the options.
		"text before the options": "Requesting permission for:\n   ls\nRun this command?\nsome prose\n  1. Yes\n  2. No\n  ↑/↓ Navigate\n",
		// Numbering that is not 1..n cannot be mapped onto digits.
		"gapped numbering": "Requesting permission for:\n   ls\nRun this command?\n> 1. Yes\n  3. No\n  ↑/↓ Navigate\n",
		"one option":       "Requesting permission for:\n   ls\nRun this command?\n> 1. Yes\n  ↑/↓ Navigate\n",
	} {
		if f, ok := ParseAgyApproval(pane); ok {
			t.Errorf("%s: parsed as a live approval: %+v", name, f)
		}
	}
	if _, ok := ParseAgyApproval(form + "esc to cancel               Gemini 3.6 Flash · low\n\n"); !ok {
		t.Error("control: the live form (status bar and blank lines below it) must parse")
	}
}

func TestAgyApprovalPermissionVerb(t *testing.T) {
	for _, tc := range []struct {
		form AgyApprovalForm
		want string
	}{
		{AgyApprovalForm{Verb: "run", Command: "ls -la /tmp"}, "run command: ls -la /tmp"},
		{AgyApprovalForm{Verb: "access", Target: "Read: /etc/hostname", Reason: "outside workspace"},
			"access file (Read: /etc/hostname; outside workspace)"},
		{AgyApprovalForm{Verb: "create", Reason: "outside workspace"}, "create file (outside workspace)"},
		{AgyApprovalForm{Verb: "edit"}, "edit file"},
	} {
		if got := tc.form.PermissionVerb(); got != tc.want {
			t.Errorf("PermissionVerb(%+v) = %q, want %q", tc.form, got, tc.want)
		}
	}
	// An unverified noun still names its verb rather than failing the parse.
	pane := "Reason: outside workspace\n\nAllow deletion of this file?\n> 1. Yes, allow deletion\n  2. No, deny deletion\n\n  ↑/↓ Navigate\n"
	if f, ok := ParseAgyApproval(pane); !ok || f.Verb != "delete" {
		t.Errorf("deletion prompt: ok=%v form=%+v", ok, f)
	}
}

func TestParseAgyMCQ(t *testing.T) {
	pane := "? What is your favourite pet?\nQuestion\n────────────\nQuestion 2/2: What is your favourite pet?\n" +
		"> 1. Cat\n  2. Dog\n  3. Write-in...\n\n  ↑/↓ Navigate · ← Back · enter Select · esc Skip\n" +
		"esc to cancel                                  Gemini 3.6 Flash\n"
	f, ok := ParseAgyMCQ(pane)
	if !ok {
		t.Fatal("the live question form must parse")
	}
	if f.Current != 2 || f.Total != 2 || f.Question != "What is your favourite pet?" || f.WriteIn != "3" {
		t.Errorf("form = %+v", f)
	}
	if got := strings.Join(NumberedOptionLabels(f.Options), "|"); got != "Cat|Dog" {
		t.Errorf("options = %q, want Cat|Dog (Write-in asks for text, not a choice)", got)
	}
	// The slash-command popup shares "↑/↓ Navigate · enter Select" but has no
	// question and no "esc Skip".
	popup := "> /effort\n────\n> /effort  Set the reasoning effort\n\n  ↑/↓ Navigate · enter Select · tab Complete\nesc to cancel\n"
	if _, ok := ParseAgyMCQ(popup); ok {
		t.Error("the slash popup is not a question form")
	}
	// An answered form in scrollback, with the composer below it.
	answered := strings.Replace(pane, "esc to cancel                                  Gemini 3.6 Flash\n",
		"> Cat\n  Thank you!\n"+agyComposer, 1)
	if _, ok := ParseAgyMCQ(answered); ok {
		t.Error("an answered form in scrollback must not read as live")
	}
	// A form whose every row is the write-in has no choice to offer.
	onlyWriteIn := "Question 1/1: Name it?\n> 1. Write-in...\n  2. Write-in…\n\n  ↑/↓ Navigate · enter Select · esc Skip\n"
	if _, ok := ParseAgyMCQ(onlyWriteIn); ok {
		t.Error("a form with no real option is not a choice")
	}
}

func TestAgyErrorFormIsAnchoredToTheLastItem(t *testing.T) {
	interrupted := "> Write an essay\n\n  ⎿  Interrupted · What should Antigravity CLI do instead?\n" + agyComposer
	eligibility := "> say hi\n\n⚠ Eligibility Check\n  ⎿  Eligibility check failed: Post\n     \"https://x/y\": proxyconnect\n\n" + agyComposer
	for pane, want := range map[string]string{interrupted: AgyErrorInterrupted, eligibility: AgyErrorEligibilityCheck} {
		if kind, ok := AgyErrorForm(pane); !ok || kind != want {
			t.Errorf("AgyErrorForm = %q/%v, want %q for:\n%s", kind, ok, want, pane)
		}
	}
	later := "\n> retry please\n\n  Done, it worked.\n"
	for name, pane := range map[string]string{
		"interrupt followed by a newer turn": strings.Replace(interrupted, "\n────", later+"────", 1),
		"check followed by a newer turn":     strings.Replace(eligibility, "\n\n────", "\n"+later+"────", 1),
		"a warning is not an error":          "⚠ Warning\n  ⎿  model x is not recognized. Using \"Gemini 3.8 Flash (High)\" instead.\n\n" + agyComposer,
		"no composer below (a modal stands)": "  ⎿  Interrupted · What should Antigravity CLI do instead?\n\n  ↑/↓ Navigate\n",
		"narrated interrupt mid-line":        "  I saw Interrupted · What should Antigravity CLI do instead? earlier\n" + agyComposer,
	} {
		if kind, ok := AgyErrorForm(pane); ok {
			t.Errorf("%s: classified as error %q", name, kind)
		}
	}
}

func TestAgyHeldForm(t *testing.T) {
	for name, tc := range map[string]struct {
		pane string
		want string
	}{
		"survey above the composer": {"  Done.\n How's the CLI experience so far? Help us improve:\n [1] Good  [2] Fine  [3] Bad  [0] Skip\n? for shortcuts        Gemini 3.6 Flash · low\n", AgyHeldSurvey},
		"curly-apostrophe survey":   {" How’s the CLI experience so far?\n [1] Good  [2] Fine  [3] Bad  [0] Skip\n", AgyHeldSurvey},
		"slash popup":               {"> /model  Set a model\n\n  ↑/↓ Navigate · enter Select · tab Complete\nesc to cancel\n", AgyHeldSlashPopup},
		"a panel":                   {"Settings\n  Search:\n> Agent Mode    default\nKeyboard: ↑/↓ Navigate  enter Select  esc Clear Search/Exit\n\n              Gemini 3.6 Flash\n", AgyHeldPanel},
	} {
		if got, ok := AgyHeldForm(tc.pane); !ok || got != tc.want {
			t.Errorf("%s: AgyHeldForm = %q/%v, want %q", name, got, ok, tc.want)
		}
	}
	for name, pane := range map[string]string{
		// A panel legend further up is history once the composer is back.
		"panel legend in scrollback": "Keyboard: ↑/↓ Navigate  esc Close\n  ⎿  Exited /config command\n" + agyComposer,
		"survey text in scrollback":  " How's the CLI experience so far?\n [0] Skip\n> next prompt\n  answer\n  more\n  lines\n  here\n" + agyComposer,
		"an ordinary idle screen":    "> hi\n  Hello!\n" + agyComposer,
	} {
		if kind, ok := AgyHeldForm(pane); ok {
			t.Errorf("%s: held as %q", name, kind)
		}
	}
}

func TestStripAgyChrome(t *testing.T) {
	banner := "root ➜ /tmp/x $ agy --model gemini-3.6-flash-low\n\n" +
		"      ▄▀▀▄        Antigravity CLI 1.2.1\n" +
		"     ▀▀▀▀▀▀       you@example.com (Google AI Pro)\n" +
		"    ▀▀▀▀▀▀▀▀      Gemini 3.6 Flash (Low)\n" +
		"   ▄▀▀    ▀▀▄     /tmp/x\n" +
		"  ▄▀▀      ▀▀▄\n\n"
	body := "────────────────\n> list it\n\n● Bash(ls -la /tmp/x) (ctrl+o to expand)\n\n  The directory is empty.\n\n" +
		"                                   1 artifact · /artifact to review\n"
	got := StripAgyChrome(banner + body + agyComposer)
	for _, chrome := range []string{"Antigravity CLI", "you@example.com", "Gemini", "? for shortcuts", "agy --model",
		"ctrl+o to expand", "/artifact to review", "▀", "────"} {
		if strings.Contains(got, chrome) {
			t.Errorf("chrome %q survived:\n%s", chrome, got)
		}
	}
	for _, content := range []string{"> list it", "● Bash(ls -la /tmp/x)", "The directory is empty."} {
		if !strings.Contains(got, content) {
			t.Errorf("content %q was stripped:\n%s", content, got)
		}
	}

	// A capture scrolled past the product-name row still opens with the logo's
	// lower rows: those go too.
	fragment := "     ▀▀▀▀▀▀       you@example.com (Google AI Pro)\n    ▀▀▀▀▀▀▀▀      Gemini 3.6 Flash (Low)\n" + body
	if got := StripAgyChrome(fragment); strings.Contains(got, "Gemini") || strings.Contains(got, "you@example.com") {
		t.Errorf("banner fragment survived:\n%s", got)
	}

	// Only chrome on screen: a fixed identity rather than an empty salient.
	if got := StripAgyChrome(banner + agyComposer); got != agyChromeOnly {
		t.Errorf("chrome-only screen = %q, want the fixed placeholder", got)
	}

	// Content that merely resembles chrome survives: one glyph row the agent
	// printed, a QR-style block using █, a menu caret that is not last.
	for _, keep := range []string{
		"  ▄▀▀▄  is the logo I drew\n> hi\n" + agyComposer,
		"▄▄▄▄▄▄▄ █▀▀▀█ ▄▄▄\n█ ▄▄▄ █ ▀▄█▀ █\n" + agyComposer,
		"Run this command?\n> 1. Yes, run command\n  2. No, cancel\n  ↑/↓ Navigate\n",
	} {
		first := strings.SplitN(keep, "\n", 2)[0]
		if !strings.Contains(StripAgyChrome(keep), strings.TrimSpace(first)) {
			t.Errorf("content line %q was stripped", first)
		}
	}
}

func TestAgyReplyWithheld(t *testing.T) {
	for _, tc := range []struct {
		st    SituationType
		agent string
		want  bool
	}{
		{SituationApproval, "agy", true},
		{SituationChoice, "antigravity", true},
		{SituationError, "agy", false}, // free text into the composer
		{SituationIdle, "agy", false},  // a hand-out
		{SituationApproval, "claude", false},
		{SituationChoice, "codex", false},
	} {
		if got := AgyReplyWithheld(tc.st, tc.agent); got != tc.want {
			t.Errorf("AgyReplyWithheld(%s, %s) = %v, want %v", tc.st, tc.agent, got, tc.want)
		}
	}
}
