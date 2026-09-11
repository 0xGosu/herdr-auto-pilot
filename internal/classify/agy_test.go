package classify

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// agyCase is what one recorded agy screen (docs/designer/agy-support.md)
// must classify as. held names the domain.AgyHeldForm kind for the screens
// hap must never answer.
type agyCase struct {
	typ     domain.SituationType
	verb    string
	options []string
	errSum  string
	held    string
}

const (
	agyRunLS = "run command: ls -la /tmp/agyscratch.H2CE"
	agyRunRM = "run command: rm -rf /tmp/agyscratch.H2CE/hello.txt"
)

var (
	agyLSOptions = []string{
		"Yes, run command",
		"Yes, and always allow in this conversation for commands that start with 'ls'",
		"Yes, and always allow for commands that start with 'ls' (Persist to settings.json)",
		"No, cancel",
	}
	agyRMOptions = []string{
		"Yes, run command",
		"Yes, and always allow in this conversation for commands that start with 'rm -rf /tmp/agyscratch.H2CE/hello.txt'",
		"Yes, and always allow for commands that start with 'rm -rf /tmp/agyscratch.H2CE/hello.txt' (Persist to settings.json)",
		"No, cancel",
	}
)

// agyFixtures covers EVERY *_agy_* transcript; TestAgyFixturesAreAllCovered
// fails when a new one is recorded without an expectation here.
var agyFixtures = map[string]agyCase{
	"approval_agy_artifact_review.txt": {typ: domain.SituationApproval, verb: domain.PermissionVerbAgyReview},
	"approval_agy_file_access.txt": {typ: domain.SituationApproval,
		verb:    "access file (Read: /etc/hostname; outside workspace)",
		options: []string{"Yes, allow access", "Yes, and always allow non-workspace access", "No, deny access"}},
	"approval_agy_file_create.txt": {typ: domain.SituationApproval,
		verb:    "create file (outside workspace)",
		options: []string{"Yes, allow creation", "Yes, and always allow non-workspace access", "No, deny creation"}},
	"approval_agy_shell.txt":        {typ: domain.SituationApproval, verb: agyRunLS, options: agyLSOptions},
	"approval_agy_shell_recent.txt": {typ: domain.SituationApproval, verb: agyRunLS, options: agyLSOptions},
	"approval_agy_shell_amend.txt":  {typ: domain.SituationUnclassifiable, held: domain.AgyHeldAmend},
	"approval_agy_shell_plan_mode.txt": {typ: domain.SituationApproval,
		verb: `run command: echo "ok" > /tmp/agyscratch.H2CE/plan.txt`,
		options: []string{
			"Yes, run command",
			"Yes, and always allow in this conversation for commands that start with 'echo'",
			"Yes, and always allow for commands that start with 'echo' (Persist to settings.json)",
			"No, cancel",
		}},
	"approval_agy_shell_wide.txt":    {typ: domain.SituationApproval, verb: agyRunRM, options: agyRMOptions},
	"approval_agy_shell_wrapped.txt": {typ: domain.SituationApproval, verb: agyRunRM, options: agyRMOptions},
	"approval_agy_trust_folder.txt": {typ: domain.SituationApproval, verb: domain.PermissionVerbAgyTrust,
		options: []string{"Yes, I trust this folder", "No, exit"}},
	"choice_agy_mcq.txt":             {typ: domain.SituationChoice, options: []string{"Apple", "Banana", "Cherry"}},
	"choice_agy_mcq_two.txt":         {typ: domain.SituationChoice, options: []string{"Red", "Green", "Blue"}},
	"choice_agy_mcq_two_recent.txt":  {typ: domain.SituationChoice, options: []string{"Red", "Green", "Blue"}},
	"choice_agy_mcq_two_q2.txt":      {typ: domain.SituationChoice, options: []string{"Cat", "Dog"}},
	"error_agy_interrupted.txt":      {typ: domain.SituationError, errSum: domain.AgyErrorInterrupted},
	"error_agy_offline.txt":          {typ: domain.SituationError, errSum: domain.AgyErrorEligibilityCheck},
	"error_agy_model_warning.txt":    {typ: domain.SituationIdle}, // a warning: the agent is usable
	"idle_agy_after_turn.txt":        {typ: domain.SituationIdle},
	"idle_agy_composer_draft.txt":    {typ: domain.SituationIdle},
	"idle_agy_declined.txt":          {typ: domain.SituationIdle},
	"idle_agy_fresh.txt":             {typ: domain.SituationIdle},
	"idle_agy_mode_accept_edits.txt": {typ: domain.SituationIdle},
	"idle_agy_mode_plan.txt":         {typ: domain.SituationIdle},
	"idle_agy_effort_picker.txt":     {typ: domain.SituationUnclassifiable, held: domain.AgyHeldPanel},
	"idle_agy_model_picker.txt":      {typ: domain.SituationUnclassifiable, held: domain.AgyHeldPanel},
	"idle_agy_shortcuts_overlay.txt": {typ: domain.SituationUnclassifiable, held: domain.AgyHeldPanel},
	"idle_agy_slash_popup.txt":       {typ: domain.SituationUnclassifiable, held: domain.AgyHeldSlashPopup},
	"idle_agy_signin_method.txt":     {typ: domain.SituationUnclassifiable, held: domain.AgyHeldSignInMethod},
	"idle_agy_signin_url.txt":        {typ: domain.SituationUnclassifiable, held: domain.AgyHeldSignInOAuth},
	"idle_agy_terms.txt":             {typ: domain.SituationUnclassifiable, held: domain.AgyHeldTerms},
	"working_agy_spinner.txt":        {typ: domain.SituationIdle}, // a finished turn once herdr says idle
}

func readAgyFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "transcripts", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestAgyFixturesAreAllCovered(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join("testdata", "transcripts"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "_agy_") {
			if _, ok := agyFixtures[e.Name()]; !ok {
				t.Errorf("%s has no expectation in agyFixtures", e.Name())
			}
		}
	}
}

// TestAgyFixturesClassify runs every recorded agy screen at the statuses herdr
// actually reports for them — idle and done, never blocked — and pins the
// situation, the permission verb, the option set and the error summary.
func TestAgyFixturesClassify(t *testing.T) {
	c := New(nil)
	for name, want := range agyFixtures {
		pane := readAgyFixture(t, name)
		for _, status := range []string{"idle", "done"} {
			s := c.Classify("agy", status, pane)
			if s.Type != want.typ {
				t.Errorf("%s @%s: type = %s, want %s", name, status, s.Type, want.typ)
				continue
			}
			if s.PermissionVerb != want.verb {
				t.Errorf("%s @%s: verb = %q, want %q", name, status, s.PermissionVerb, want.verb)
			}
			if len(s.Options) != 0 || len(want.options) != 0 {
				if !reflect.DeepEqual(s.Options, want.options) {
					t.Errorf("%s @%s: options = %q, want %q", name, status, s.Options, want.options)
				}
			}
			if s.ErrorSummary != want.errSum {
				t.Errorf("%s @%s: error summary = %q, want %q", name, status, s.ErrorSummary, want.errSum)
			}
			if s.MCQKind != "" || s.EffectiveAnswerCount() > 1 {
				// A kind or a count routes the form into the multi-question
				// sweep and the Claude/Codex deliverers, which press arrow keys
				// (see domain.MCQAgyQuestions).
				t.Errorf("%s @%s: MCQKind=%q AnswerCount=%d, want neither set", name, status, s.MCQKind, s.EffectiveAnswerCount())
			}
		}
		held, ok := domain.AgyHeldForm(pane)
		if (want.held != "") != ok || held != want.held {
			t.Errorf("%s: AgyHeldForm = %q/%v, want %q", name, held, ok, want.held)
		}
	}
}

// The forms stay what they are should herdr ever report agy blocked (its
// manifest rule may be fixed), and auto-accept's held-still re-check
// re-classifies a non-idle row as blocked.
func TestAgyFormsClassifyWhenBlocked(t *testing.T) {
	c := New(nil)
	for name, want := range agyFixtures {
		if want.typ != domain.SituationApproval && want.typ != domain.SituationChoice && want.typ != domain.SituationError {
			continue
		}
		if s := c.Classify("agy", "blocked", readAgyFixture(t, name)); s.Type != want.typ {
			t.Errorf("%s @blocked: type = %s, want %s", name, s.Type, want.typ)
		}
	}
}

// Working is excluded from the parked statuses, exactly as for Codex's Plan
// approval and Claude's remote-environment picker.
func TestAgyFormsNotParkedWhileWorking(t *testing.T) {
	c := New(nil)
	for name, want := range agyFixtures {
		if want.typ != domain.SituationApproval && want.typ != domain.SituationChoice {
			continue
		}
		if s := c.Classify("agy", "working", readAgyFixture(t, name)); s.Type == want.typ {
			t.Errorf("%s @working classified %s; only idle/done/blocked may park a form", name, s.Type)
		}
	}
}

// The same text from another agent is narration, not a license to act.
func TestAgyFormsAreAgyScoped(t *testing.T) {
	c := New(nil)
	for name, want := range agyFixtures {
		if want.typ == domain.SituationIdle {
			continue
		}
		pane := readAgyFixture(t, name)
		for _, agent := range []string{"claude", "codex", "gemini"} {
			if s := c.Classify(agent, "done", pane); s.Type != domain.SituationIdle {
				t.Errorf("%s as %s: type = %s, want idle (agy forms are agy-scoped)", name, agent, s.Type)
			}
		}
	}
}

// One approval yields one signature whatever the pane's width and whichever
// read produced the capture — otherwise every resize or read source would
// mint a new rule for the same prompt.
func TestAgySignatureIndependentOfWidthAndReadSource(t *testing.T) {
	c := New(nil)
	sig := func(name string) domain.SignatureResult {
		return domain.ComputeSignature(c.Classify("agy", "done", readAgyFixture(t, name)))
	}
	for _, pair := range [][2]string{
		{"approval_agy_shell_wide.txt", "approval_agy_shell_wrapped.txt"},
		{"approval_agy_shell.txt", "approval_agy_shell_recent.txt"},
		{"choice_agy_mcq_two.txt", "choice_agy_mcq_two_recent.txt"},
	} {
		a, b := sig(pair[0]), sig(pair[1])
		if a.Verdict != domain.GuardOK || a.Signature != b.Signature {
			t.Errorf("%s and %s must share a signature:\n %q\n %q", pair[0], pair[1], a.Salient, b.Salient)
		}
	}
	// ...and a different command is a different rule.
	if sig("approval_agy_shell.txt").Signature == sig("approval_agy_shell_wide.txt").Signature {
		t.Error("an ls approval and an rm approval must not share a signature")
	}
}

// agy's banner, model line and status bar differ per session; left in the
// idle salient they would split one situation into a signature per model and
// spend the salient window on chrome.
func TestAgyIdleSalientStripsChrome(t *testing.T) {
	c := New(nil)
	for name, want := range agyFixtures {
		if want.typ != domain.SituationIdle {
			continue
		}
		pane := readAgyFixture(t, name)
		sig := domain.ComputeSignature(c.Classify("agy", "done", pane))
		if sig.Verdict != domain.GuardOK {
			t.Errorf("%s: over-masked (%q) — an idle agy would escalate before its task source is read", name, sig.Salient)
			continue
		}
		for _, chrome := range []string{"Antigravity CLI", "operator@example.com", "Gemini 3.6 Flash", "? for shortcuts",
			"esc to cancel", "ctrl+o to expand", "agy --model", "▀"} {
			if strings.Contains(sig.Salient, chrome) {
				t.Errorf("%s: chrome %q in the salient %q", name, chrome, sig.Salient)
			}
		}
		swapped := strings.NewReplacer("Gemini 3.6 Flash (Low)", "Claude Opus 4.6 (Thinking)",
			"Gemini 3.6 Flash · low", "Claude Opus 4.6 (Thinking) · high",
			"Gemini 3.6 Flash", "Claude Opus 4.6 (Thinking)").Replace(pane)
		if other := domain.ComputeSignature(c.Classify("agy", "done", swapped)); other.Signature != sig.Signature {
			t.Errorf("%s: switching the model changed the signature:\n %q\n %q", name, sig.Salient, other.Salient)
		}
	}
}

// Setup and operator-UI screens are held ahead of every rule, the operator's
// included: a rule matching text on them can only be wrong about them.
func TestAgyHeldScreensIgnoreOperatorRules(t *testing.T) {
	c := New([]config.ClassifierRule{
		{AgentType: "*", Situation: "idle", Keywords: []string{"Terms of Service", "Keyboard:"}},
		{AgentType: "agy", Situation: "approval", Keywords: []string{"login method", "authorization code"}},
	})
	for name, want := range agyFixtures {
		if want.held == "" {
			continue
		}
		if s := c.Classify("agy", "idle", readAgyFixture(t, name)); s.Type != domain.SituationUnclassifiable {
			t.Errorf("%s: an operator rule turned a held screen into %s", name, s.Type)
		}
	}
}
