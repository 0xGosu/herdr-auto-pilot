package domain

import (
	"errors"
	"fmt"
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

func TestAgyFormSituation(t *testing.T) {
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
		if got := AgyFormSituation(tc.st, tc.agent); got != tc.want {
			t.Errorf("AgyFormSituation(%s, %s) = %v, want %v", tc.st, tc.agent, got, tc.want)
		}
	}
}

// The composer proof every agy hand-out path asks: ready means an EMPTY
// composer at rest, and every other shape — however idle herdr calls it — is
// not. (The recorded screens are run through it in internal/deliver.)
func TestAgyComposerReady(t *testing.T) {
	const rule = "────────────────────────────────────────"
	const bar = "? for shortcuts                                         Gemini 3.6 Flash · low"
	for name, tc := range map[string]struct {
		pane string
		want bool
	}{
		"empty composer":        {agyComposer, true},
		"trailing blank lines":  {agyComposer + "\n\n", true},
		"plan mode placeholder": {rule + "\n> Plan mode: research & plan only (shift+tab to cycle)\n" + rule + "\n" + bar, true},
		"accept-edits placeholder": {rule + "\n> Accept-edits mode: file edits auto-approved (shift+tab to cycle)\n" + rule + "\n" + bar,
			true},
		// A draft drops the "? for shortcuts" token; the draft text alone is
		// also refused, in case a build keeps the token.
		"draft":             {rule + "\n> hello draft\n" + rule + "\n                                   Gemini 3.6 Flash · low", false},
		"draft, token kept": {rule + "\n> hello draft\n" + rule + "\n" + bar, false},
		"working turn":      {rule + "\n>\n" + rule + "\nesc to cancel                              Gemini 3.6 Flash · low", false},
		// The feedback survey stands ABOVE a ready composer, and a digit typed
		// while it does answers the survey.
		"survey": {"How's the CLI experience so far? Help us improve:\n[1] Good  [2] Fine  [3] Bad  [0] Skip\n\n" +
			agyComposer, false},
		// Rendered on one row, as the capture's very first line.
		"survey on the first row": {"How's the CLI experience so far? Help us improve: [1] Good  [2] Fine  [3] Bad  [0] Skip\n" +
			agyComposer, false},
		// A panel drawn below the composer pushes the status bar away from it.
		"panel below the composer": {rule + "\n>\n" + rule + "\nAction required (1 left)\n› □ new plan.md\n\n" + bar, false},
		"no composer":              {"  Some agent output\n\n" + bar, false},
		"nothing":                  {"", false},
	} {
		if got := AgyComposerReady(tc.pane); got != tc.want {
			t.Errorf("%s: AgyComposerReady = %v, want %v", name, got, tc.want)
		}
	}
}

func TestAgyAnswerKey(t *testing.T) {
	approval := AgyForm{Kind: AgyFormApproval, Options: []NumberedOption{
		{"1", "Yes, run command"}, {"2", "Yes, and always allow in this conversation"}, {"4", "No, cancel"}}}
	question := AgyForm{Kind: AgyFormQuestion, WriteIn: "4", Options: []NumberedOption{
		{"1", "Apple"}, {"2", "Banana"}, {"3", "Cherry"}}}
	review := AgyForm{Kind: AgyFormReview}
	for _, tc := range []struct {
		form          AgyForm
		reply, key    string
		notAnswerable bool
	}{
		{form: approval, reply: "Yes, run command", key: "1"},
		{form: approval, reply: "no, CANCEL", key: "4"},
		{form: approval, reply: "4", key: "4"},
		{form: approval, reply: "Yes, allow everything"}, // offered by no option
		{form: approval, reply: "Yes"},                   // a prefix of two options
		{form: question, reply: "Banana", key: "2"},
		{form: question, reply: "Write-in...", notAnswerable: true},
		{form: question, reply: "4", notAnswerable: true},
		{form: review, reply: "Approve", key: "y"},
		{form: review, reply: "n", key: "n"},
		{form: review, reply: "shift+a"}, // approve ALL is never a reply
	} {
		key, err := AgyAnswerKey(tc.form, tc.reply)
		switch {
		case tc.notAnswerable:
			if !errors.Is(err, ErrAgyNotAnswerable) {
				t.Errorf("%s %q: err = %v, want ErrAgyNotAnswerable", tc.form.Kind, tc.reply, err)
			}
		case tc.key == "":
			if err == nil {
				t.Errorf("%s %q: key %q, want a refusal", tc.form.Kind, tc.reply, key)
			}
		case err != nil || key != tc.key:
			t.Errorf("%s %q: key %q err %v, want %q", tc.form.Kind, tc.reply, key, err, tc.key)
		}
	}
}

// Moving the trust prompt's caret does not make it a different prompt — the
// check a caret press relies on — while the next question of a form, even
// with identical labels, is a different form.
func TestAgyFormSameAs(t *testing.T) {
	trust := "Do you trust the contents of this project?\n\n> Yes, I trust this folder\n  No, exit\n\n" +
		"  ↑/↓ Navigate · enter Confirm\n"
	moved := strings.Replace(trust, "> Yes, I trust this folder\n  No, exit", "  Yes, I trust this folder\n> No, exit", 1)
	a, okA := ParseAgyForm(trust)
	b, okB := ParseAgyForm(moved)
	if !okA || !okB || a.Caret != 0 || b.Caret != 1 || !a.SameAs(b) {
		t.Errorf("trust caret move: %+v %v / %+v %v, want the same prompt with the caret on rows 1 and 2", a, okA, b, okB)
	}
	q := func(i int) string {
		return fmt.Sprintf("Question %d/2: Pick one\n\n> 1. Yes\n  2. No\n\n  ↑/↓ Navigate · enter Select · esc Skip\n", i)
	}
	q1, _ := ParseAgyForm(q(1))
	q2, _ := ParseAgyForm(q(2))
	if q1.Kind != AgyFormQuestion || q1.SameAs(q2) {
		t.Errorf("question 1 and question 2 must be different forms: %+v / %+v", q1, q2)
	}
}

const agyRule = "────────────────────────────────────────"

// agyDraftScreen renders a capture whose composer holds body, with the bottom
// chrome agy paints while a draft stands: the model segment WITHOUT the
// "? for shortcuts" token, which agy drops as soon as anything is typed.
func agyDraftScreen(body string) string {
	return "  Working on it.\n\n" + agyRule + "\n" + body + "\n" + agyRule + "\n" +
		"                                                        Gemini 3.6 Flash · low\n"
}

// AgyComposerDraft is what tells "the hand-out is queued, unsent" apart from
// "the agent took it and is working" — AgyComposerReady collapses both into
// "not ready", which is the right answer before a send and useless after one.
func TestAgyComposerDraftReadsBackQueuedText(t *testing.T) {
	for _, tc := range []struct {
		name string
		pane string
		want string
	}{
		{"empty composer is not a draft", agyComposer, ""},
		{"operator's draft", agyDraftScreen("> half-typed thought"), "half-typed thought"},
		// The whole reason the block is read between the rules rather than at a
		// fixed offset: a hand-out is long and agy wraps it.
		{"a wrapped multi-line hand-out is rejoined",
			agyDraftScreen("> Next task: run the suite\n  and report back"),
			"Next task: run the suite and report back"},
		// A mode placeholder is agy's own text in an EMPTY composer, not a draft.
		{"mode placeholder is not a draft",
			agyDraftScreen("> Plan mode: research & plan only (shift+tab to cycle)"), ""},
		// No composer sandwich at the bottom at all.
		{"standing approval", agyApprovalScreen(), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, verdict := AgyComposerDraft(tc.pane, agyComposerScanLimit)
			if tc.want == "" {
				if verdict != AgyDraftNone {
					t.Fatalf("AgyComposerDraft = %q, %v; want no draft", got, verdict)
				}
				return
			}
			if verdict != AgyDraftPresent || got != tc.want {
				t.Errorf("AgyComposerDraft = %q, %v; want %q, AgyDraftPresent", got, verdict, tc.want)
			}
		})
	}
}

// A hand-out taller than the scan window must read as UNREADABLE, never as "no
// draft". The two used to be the same answer, and the caller's ledger path —
// safe only when the item really was taken up — then ran for precisely the
// LARGE hand-outs, returning an item to "[ ]" while its queued copy was still on
// its way to the same agent.
//
// The control is the same pane read with a window wide enough for it: without
// it this test would pass just as happily if the parser had stopped recognizing
// the screen at all.
func TestATallComposerBlockIsUnreadableRatherThanAbsent(t *testing.T) {
	body := "> Next task: run the suite"
	for i := 0; i < 40; i++ {
		body += "\n  and then report back on what failed"
	}
	pane := agyDraftScreen(body)

	if got, verdict := AgyComposerDraft(pane, agyComposerScanLimit); verdict != AgyDraftUnreadable {
		t.Errorf("with a %d-line window: = %q, %v; want AgyDraftUnreadable — "+
			"reading a too-tall composer as 'no draft' is what sends the caller down the ledger path",
			agyComposerScanLimit, got, verdict)
	}
	// Control: the SAME pane, with the window a caller who knows what it sent
	// would compute, reads the draft back.
	sent := "Next task: run the suite" + strings.Repeat(" and then report back on what failed", 40)
	got, verdict := AgyComposerDraft(pane, AgyDraftScanLimit(sent))
	if verdict != AgyDraftPresent || !strings.HasPrefix(got, "Next task: run the suite and then") {
		t.Errorf("with the scaled window: = %.60q, %v; want the draft back", got, verdict)
	}
}

// The scan window has to reach past a realistic hand-out, which wraps well
// beyond 20 lines, while still being bounded.
func TestAgyDraftScanLimitScalesWithWhatWasSent(t *testing.T) {
	if got := AgyDraftScanLimit("go"); got != agyComposerScanLimit {
		t.Errorf("AgyDraftScanLimit(short) = %d, want the %d floor", got, agyComposerScanLimit)
	}
	// A rendered task prompt: boilerplate plus the task text.
	handout := strings.Repeat("Next task: run the full suite and report back. ", 20)
	got := AgyDraftScanLimit(handout)
	if want := len(handout) / agyDraftWrapWidth; got < want {
		t.Errorf("AgyDraftScanLimit(handout) = %d, want at least %d lines — a window shorter than "+
			"the text can wrap to is unreachable for exactly the common case", got, want)
	}
	if got := AgyDraftScanLimit(strings.Repeat("x", 100000)); got != agyComposerScanCeiling {
		t.Errorf("AgyDraftScanLimit(huge) = %d, want the %d ceiling", got, agyComposerScanCeiling)
	}
}

// agyApprovalScreen is agy's shell approval — the screen a hand-out must never
// be typed into, and one with no composer sandwich at the bottom.
func agyApprovalScreen() string {
	return "Requesting permission for:\n   go test ./...\n\nRun this command?\n" +
		"> 1. Yes, run command\n" +
		"  2. Yes, and always allow in this conversation for commands that start with 'go'\n" +
		"  3. Yes, and always allow for commands that start with 'go' (Persist to settings.json)\n" +
		"  4. No, cancel\n\n" +
		"  ↑/↓ Navigate · tab Amend · ctrl+g edit/expand command\n" +
		"esc to cancel                                              Gemini 3.6 Flash · low\n"
}

// Attribution decides whether a task is stranded at "[-]" for a human, so an
// operator's own draft must never be mistaken for hap's hand-out.
func TestAgyDraftMatchesOnlyTheTextHapSent(t *testing.T) {
	sent := "Next task: run the suite and report back when it is green"
	for _, tc := range []struct {
		name  string
		draft string
		want  bool
	}{
		{"the whole hand-out", sent, true},
		{"the head of a wrapped hand-out", "Next task: run the suite", true},
		{"the operator's own note", "remember to check the flake", false},
		// A containment test would pass this one: "suite" appears in what was
		// sent. Only a PREFIX proves the composer is holding our message.
		{"a fragment from the middle", "suite and report back", false},
		{"nothing typed", "", false},
		// The window is a FLOOR too. One or two characters the operator typed
		// in the moment after the send can open the hand-out text by accident;
		// attributing those to hap strands the task at "[-]" with no ledger
		// row, the outcome this function exists to avoid.
		{"one character the operator just typed", "N", false},
		{"a few characters that happen to open the hand-out", "Next t", false},
		// The wrap falls wherever the column runs out, so the composer's first
		// line can end mid-word. Rejoining the block with a space used to
		// insert a separator that is not in what was sent, so a genuinely
		// queued hand-out matched nothing and took the ledger path.
		{"a wrap that fell mid-word", "Next task: run the su ite and report", true},
		{"a narrow pane wrapping every few words",
			"Next\ntask: run\nthe suite and\nreport back", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := AgyDraftMatches(tc.draft, sent); got != tc.want {
				t.Errorf("AgyDraftMatches(%q) = %v, want %v", tc.draft, got, tc.want)
			}
		})
	}

	// The floor must not refuse a SHORT hand-out that really is sitting there:
	// a flat "refuse anything under the window" would, and that is the costly
	// direction — the item gets a ledger row and is re-offered while the queued
	// copy is still on its way.
	t.Run("a short hand-out queued in full", func(t *testing.T) {
		if !AgyDraftMatches("Next task: fix #12", "Next task: fix #12") {
			t.Error("a draft that is the WHOLE of what was sent must match however short it is")
		}
	})
}
