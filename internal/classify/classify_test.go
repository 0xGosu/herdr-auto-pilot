package classify

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// TestGoldenTranscripts pins classification of recorded pane transcripts
// (Testing Strategy: golden tests for FR-002). Regenerate with
// UPDATE_GOLDEN=1 go test ./internal/classify/.
func TestGoldenTranscripts(t *testing.T) {
	c := New(nil)

	statusFor := map[string]string{
		"idle_finished.txt":              "idle",
		"codex_idle.txt":                 "idle",
		"approval_codex_plan.txt":        "idle",
		"error_codex_rate_limit.txt":     "idle",
		"error_codex_usage_limit.txt":    "idle",
		"error_codex_banner.txt":         "idle",
		"approval_claude_remote_env.txt": "idle",
	}
	agentTypeFor := map[string]string{
		"approval_codex_plan.txt":     "codex",
		"codex_idle.txt":              "codex",
		"choice_codex_mcq.txt":        "codex",
		"error_codex_interrupted.txt": "codex",
		"error_codex_rate_limit.txt":  "codex",
		"error_codex_usage_limit.txt": "codex",
		"error_codex_banner.txt":      "codex",
	}

	entries, err := os.ReadDir("testdata/transcripts")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	var b strings.Builder
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join("testdata", "transcripts", name))
		if err != nil {
			t.Fatal(err)
		}
		status := statusFor[name]
		if status == "" {
			status = "blocked"
		}
		agentType := agentTypeFor[name]
		if agentType == "" {
			agentType = "claude"
		}
		s := c.Classify(agentType, status, string(data))
		verb := ""
		if s.PermissionVerb != "" {
			verb = strings.Fields(s.PermissionVerb)[0]
		}
		fmt.Fprintf(&b, "%s status=%s type=%s verb=%s options=%d\n",
			name, status, s.Type, verb, len(s.Options))
	}

	goldenPath := filepath.Join("testdata", "classifications.golden")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(goldenPath, []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	golden, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatal(err)
	}
	if b.String() != string(golden) {
		t.Errorf("classification drift.\n--- got ---\n%s--- want ---\n%s", b.String(), string(golden))
	}
}

// TestApprovalSignaturesDoNotCollideAcrossScreens is the end-to-end
// regression for issue #155: a Claude plan-approval screen and a Bash command
// approval both extract verb "proceed", but their option sets differ, so
// their signatures must differ — a "Yes" rule learned on one must never
// auto-fire on the other.
func TestApprovalSignaturesDoNotCollideAcrossScreens(t *testing.T) {
	c := New(nil)
	sigFor := func(name string) domain.SignatureResult {
		t.Helper()
		data, err := os.ReadFile(filepath.Join("testdata", "transcripts", name))
		if err != nil {
			t.Fatal(err)
		}
		s := c.Classify("claude", "blocked", string(data))
		if s.Type != domain.SituationApproval {
			t.Fatalf("%s classified %v, want approval", name, s.Type)
		}
		sig := domain.ComputeSignature(s)
		if sig.Verdict != domain.GuardOK {
			t.Fatalf("%s verdict = %v, want ok (salient %q)", name, sig.Verdict, sig.Salient)
		}
		return sig
	}
	plan := sigFor("approval_claude_plan.txt")
	perm := sigFor("approval_permission.txt")
	if plan.Signature == perm.Signature {
		t.Errorf("plan approval and permission approval share a signature (salient %q)", plan.Salient)
	}
}

func TestUnclassifiableFailsSafe(t *testing.T) {
	c := New(nil)
	s := c.Classify("claude", "blocked", "completely novel pane content with no known shape")
	if s.Type != domain.SituationUnclassifiable {
		t.Errorf("unknown blocked content must be unclassifiable, got %v", s.Type)
	}
}

func TestIdleStatusWithoutContentRule(t *testing.T) {
	c := New(nil)
	s := c.Classify("claude", "idle", "just some ordinary output scroll")
	if s.Type != domain.SituationIdle {
		t.Errorf("idle agent with plain output should classify idle, got %v", s.Type)
	}
}

func TestOperatorRulesTakePriority(t *testing.T) {
	rules := []config.ClassifierRule{{
		AgentType: "custombot", Situation: "error",
		Regex: []string{`(?i)wedged`},
	}}
	c := New(rules)
	s := c.Classify("custombot", "blocked", "the pipeline is wedged again")
	if s.Type != domain.SituationError {
		t.Errorf("operator rule should classify, got %v", s.Type)
	}
	// Rule scoped to custombot must not affect other agent types.
	s = c.Classify("claude", "blocked", "the pipeline is wedged again")
	if s.Type == domain.SituationError {
		t.Error("agent-type-scoped rule leaked to another agent type")
	}
}

func TestInvalidManifestPatternFailsSafe(t *testing.T) {
	// Manifest parse error → rule skipped, classification falls through
	// (never panics, never misclassifies on a broken rule).
	rules := []config.ClassifierRule{{
		AgentType: "*", Situation: "error", Regex: []string{`([broken`},
	}}
	c := New(rules)
	s := c.Classify("claude", "blocked", "novel content")
	if s.Type != domain.SituationUnclassifiable {
		t.Errorf("broken manifest must fail safe to unclassifiable, got %v", s.Type)
	}
}

func TestChoiceOptionExtraction(t *testing.T) {
	c := New(nil)
	pane := "Select an option:\n 1. red\n 2. green\n 3. blue\n"
	s := c.Classify("claude", "blocked", pane)
	if s.Type != domain.SituationChoice || len(s.Options) != 3 || s.Options[1] != "green" {
		t.Errorf("option extraction failed: %+v", s)
	}
}

// Claude AskUserQuestion MCQ forms (single and multi-tab plan-mode) must
// classify as choice with the options extracted — previously they fell
// through to unclassifiable (bare escalation, no suggestion, no LLM).
func TestClaudeMCQFormsClassifyAsChoice(t *testing.T) {
	c := New(nil)
	for _, name := range []string{"choice_claude_mcq.txt", "choice_claude_mcq_tabs.txt"} {
		data, err := os.ReadFile(filepath.Join("testdata", "transcripts", name))
		if err != nil {
			t.Fatal(err)
		}
		s := c.Classify("claude", "blocked", string(data))
		if s.Type != domain.SituationChoice {
			t.Fatalf("%s: type = %v, want choice", name, s.Type)
		}
		if len(s.Options) != 5 {
			t.Errorf("%s: options = %d (%v), want 5", name, len(s.Options), s.Options)
		}
		// Synthetic trailing entries are real selectable options and must
		// be part of the set (digit mapping stays correct).
		joined := strings.Join(s.Options, "|")
		for _, want := range []string{"Type something.", "Chat about this"} {
			if !strings.Contains(joined, want) {
				t.Errorf("%s: options missing %q: %v", name, want, s.Options)
			}
		}
	}
}

func TestCodexMCQFormClassifiesAsChoice(t *testing.T) {
	c := New(nil)
	data, err := os.ReadFile(filepath.Join("testdata", "transcripts", "choice_codex_mcq.txt"))
	if err != nil {
		t.Fatal(err)
	}
	s := c.Classify("codex", "blocked", string(data))
	if s.Type != domain.SituationChoice {
		t.Fatalf("type = %v, want choice", s.Type)
	}
	if s.MCQKind != domain.MCQCodexQuestions || s.EffectiveAnswerCount() != 3 {
		t.Fatalf("form = %q answers=%d, want codex_questions/3", s.MCQKind, s.EffectiveAnswerCount())
	}
	if got := strings.Join(s.Options, "|"); !strings.Contains(got, "Add shared columns") || len(s.Options) != 3 {
		t.Fatalf("options = %v, want three Codex labels", s.Options)
	}
	if idle := c.Classify("codex", "idle", string(data)); idle.Type == domain.SituationChoice {
		t.Fatal("non-blocked Codex form must not classify as a live choice")
	}
	if other := c.Classify("claude", "blocked", string(data)); other.Type == domain.SituationChoice {
		t.Fatal("Codex structural form must remain scoped to agent_type codex")
	}
}

func TestClaudeMCQSingleFormRealLabelsAndNoTabs(t *testing.T) {
	c := New(nil)
	data, err := os.ReadFile(filepath.Join("testdata", "transcripts", "choice_claude_mcq.txt"))
	if err != nil {
		t.Fatal(err)
	}
	s := c.Classify("claude", "blocked", string(data))
	joined := strings.Join(s.Options, "|")
	for _, want := range []string{"All 4 as REQUEST_CHANGES", "Critical+Major only", "Hold — don't submit"} {
		if !strings.Contains(joined, want) {
			t.Errorf("options missing real label %q: %v", want, s.Options)
		}
	}
	if s.TabCount != 0 {
		t.Errorf("single-question form must not report tabs, got %d", s.TabCount)
	}
}

func TestClaudeMCQMultiTabFormReportsTabCount(t *testing.T) {
	c := New(nil)
	data, err := os.ReadFile(filepath.Join("testdata", "transcripts", "choice_claude_mcq_tabs.txt"))
	if err != nil {
		t.Fatal(err)
	}
	s := c.Classify("claude", "blocked", string(data))
	if s.TabCount != 5 {
		t.Errorf("tab count = %d, want 5 (4 questions + Submit)", s.TabCount)
	}
	joined := strings.Join(s.Options, "|")
	if !strings.Contains(joined, "never_auto (Recommended)") {
		t.Errorf("options missing focused question's labels: %v", s.Options)
	}
}

func TestClaudeMCQMultiTabV2FooterReportsTabCount(t *testing.T) {
	// Regression for #50: the Claude Code v2.1.207 footer says "Tab to switch
	// questions", not "Tab/Arrow keys to navigate". A genuine 3-tab form
	// (Test scope / Daemon / Submit) must still report its tab count.
	c := New(nil)
	data, err := os.ReadFile(filepath.Join("testdata", "transcripts", "choice_claude_mcq_tabs_v2.txt"))
	if err != nil {
		t.Fatal(err)
	}
	s := c.Classify("claude", "blocked", string(data))
	if s.TabCount != 3 {
		t.Errorf("tab count = %d, want 3 (2 questions + Submit)", s.TabCount)
	}
}

func TestClaudeMCQOneQuestionTabFormReportsTabCount(t *testing.T) {
	// Live capture (2026-07-20): a form with ONE question tab plus Submit
	// carries the tab header but the plain selection footer — there is no
	// sibling question to switch to. It was classified as a plain menu, so
	// delivery sent the bare digit, which only toggled the option's checkbox
	// and left the agent blocked on the Submit tab.
	c := New(nil)
	data, err := os.ReadFile(filepath.Join("testdata", "transcripts", "choice_claude_mcq_tabs_one_question.txt"))
	if err != nil {
		t.Fatal(err)
	}
	s := c.Classify("claude", "blocked", string(data))
	if s.Type != domain.SituationChoice {
		t.Fatalf("type = %q, want choice", s.Type)
	}
	if s.MCQKind != domain.MCQClaudeTabs {
		t.Errorf("form kind = %q, want %q", s.MCQKind, domain.MCQClaudeTabs)
	}
	if s.TabCount != 2 {
		t.Errorf("tab count = %d, want 2 (1 question + Submit)", s.TabCount)
	}
}

func TestNarratedNumberedListNotChoice(t *testing.T) {
	// A narrated markdown list whose first item wraps into a long indented
	// continuation block (> 4 lines, no MCQ footer) must NOT classify as a
	// live menu.
	pane := "Summary of the work so far:\n" +
		"1. Refactored the consumer\n" +
		"   this took several steps because the FIFO lock handling\n" +
		"   was tangled with retry classification and needed the\n" +
		"   incident doc's primary recommendation applied first,\n" +
		"   plus a new integration scenario for 2ndp-before-chargeback,\n" +
		"   and a migration for the recon table\n" +
		"2. Updated the spec\n"
	c := New(nil)
	s := c.Classify("claude", "blocked", pane)
	if s.Type == domain.SituationChoice {
		t.Fatalf("narrated list with a long continuation block must not be choice, got %v", s.Type)
	}
}

func TestApprovalAndChoiceOnlyWhenBlocked(t *testing.T) {
	// Approval and choice are BLOCKED situations: the same content at a
	// non-blocked status must NOT classify as approval/choice.
	c := New(nil)
	approval := "Do you want to proceed?\n❯ 1. Yes\n  2. No\n"
	choice := "Which option would you like?\n"
	for _, status := range []string{"working", "idle", "done"} {
		if s := c.Classify("claude", status, approval); s.Type == domain.SituationApproval {
			t.Errorf("approval content at status %q must not classify approval", status)
		}
		if s := c.Classify("claude", status, choice); s.Type == domain.SituationChoice {
			t.Errorf("choice content at status %q must not classify choice", status)
		}
	}
	// The same content when blocked still classifies.
	if s := c.Classify("claude", "blocked", approval); s.Type != domain.SituationApproval {
		t.Errorf("blocked approval content = %v, want approval", s.Type)
	}
	if s := c.Classify("claude", "blocked", choice); s.Type != domain.SituationChoice {
		t.Errorf("blocked choice content = %v, want choice", s.Type)
	}
}

func TestClaudeMCQAtNonBlockedNotChoice(t *testing.T) {
	// A Claude MCQ form (structural, no textual keyword) at a non-blocked
	// status must not read as a live choice.
	c := New(nil)
	data, err := os.ReadFile(filepath.Join("testdata", "transcripts", "choice_claude_mcq_tabs_v2.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if s := c.Classify("claude", "idle", string(data)); s.Type == domain.SituationChoice {
		t.Errorf("MCQ form at idle status must not classify choice, got %v", s.Type)
	}
}

func TestBlockedMCQWithErrorNarrationClassifiesChoice(t *testing.T) {
	// Rule order is approval > choice > error. A blocked Claude MCQ whose
	// content ALSO carries a real error signal (an interrupt line that
	// ClaudeErrorForm matches) must still classify as choice — the MCQ signal
	// is evaluated at the choice position, before the error rule. The question
	// is deliberately not approval-shaped so choice-beats-error is the only
	// thing under test.
	pane := "⎿  Interrupted · What should Claude do instead?\n\n" +
		"How should the fix be submitted?\n" +
		"❯ 1. Retry the build\n  2. Skip and continue\n\n" +
		"Enter to select · ↑/↓ to navigate · Esc to cancel\n"
	// Sanity: the interrupt line alone WOULD classify as error, so choice
	// really is winning over a live error signal.
	if _, ok := domain.ClaudeErrorForm(pane); !ok {
		t.Fatal("test setup: pane must contain a live error signal")
	}
	c := New(nil)
	if s := c.Classify("claude", "blocked", pane); s.Type != domain.SituationChoice {
		t.Fatalf("blocked MCQ with error signal = %v, want choice", s.Type)
	}
}

func TestPlainNumberedListNotChoice(t *testing.T) {
	// Numbered-menu regexes were removed: a bare numbered list with no MCQ
	// footer and no "select an option" keyword must not classify as choice,
	// even when blocked.
	c := New(nil)
	pane := "Here is what I changed:\n1. Fixed the parser\n2. Added a test\n3. Updated docs\n"
	if s := c.Classify("claude", "blocked", pane); s.Type == domain.SituationChoice {
		t.Errorf("plain numbered list must not classify choice, got %v", s.Type)
	}
}

func TestClaudeErrorSituations(t *testing.T) {
	c := New(nil)
	for _, name := range []string{"error_claude_limit.txt", "error_claude_interrupted.txt", "error_claude_api_retry.txt"} {
		data, err := os.ReadFile(filepath.Join("testdata", "transcripts", name))
		if err != nil {
			t.Fatal(err)
		}
		if s := c.Classify("claude", "blocked", string(data)); s.Type != domain.SituationError {
			t.Errorf("%s: type = %v, want error", name, s.Type)
		}
		// Error detection is claude-scoped: the same content on another agent
		// type must NOT classify as error (no rule for it yet).
		if s := c.Classify("codex", "blocked", string(data)); s.Type == domain.SituationError {
			t.Errorf("%s on non-claude agent must not classify error, got %v", name, s.Type)
		}
	}
}

func TestCodexErrorSituations(t *testing.T) {
	c := New(nil)
	cases := []struct {
		name string
		// The usage-limit fixture's "You've hit your usage limit" phrasing is
		// shared with Claude's own limit stop (claudeLimitRE), so it genuinely
		// classifies error on both agent types; every other codex form is
		// codex-scoped and must NOT classify error on claude.
		claudeAlsoErrors bool
	}{
		{"error_codex_interrupted.txt", false},
		{"error_codex_rate_limit.txt", false},
		{"error_codex_usage_limit.txt", true},
		{"error_codex_banner.txt", false},
	}
	for _, tc := range cases {
		data, err := os.ReadFile(filepath.Join("testdata", "transcripts", tc.name))
		if err != nil {
			t.Fatal(err)
		}
		if s := c.Classify("codex", "blocked", string(data)); s.Type != domain.SituationError {
			t.Errorf("%s: type = %v, want error", tc.name, s.Type)
		}
		if s := c.Classify("claude", "blocked", string(data)); (s.Type == domain.SituationError) != tc.claudeAlsoErrors {
			t.Errorf("%s on claude: type = %v, want error=%v", tc.name, s.Type, tc.claudeAlsoErrors)
		}
	}
}

// The live failure mode from issue #161: herdr reports the hard-blocked agent
// as IDLE, so error detection must not depend on a blocked status — otherwise
// a learned idle rule answers "do nothing" and the operator never hears about
// an agent that cannot progress for days.
func TestCodexErrorBannersDetectedAtIdle(t *testing.T) {
	c := New(nil)
	cases := []struct {
		name        string
		wantSummary string
	}{
		{"error_codex_usage_limit.txt", domain.CodexErrorUsageLimit},
		{"error_codex_banner.txt", "banner: stream error: connection reset by peer; retrying may help"},
	}
	for _, tc := range cases {
		data, err := os.ReadFile(filepath.Join("testdata", "transcripts", tc.name))
		if err != nil {
			t.Fatal(err)
		}
		s := c.Classify("codex", "idle", string(data))
		if s.Type != domain.SituationError {
			t.Fatalf("%s at idle: type = %v, want error", tc.name, s.Type)
		}
		// The exact stable kind is load-bearing: it is the error signature's
		// salient, so instances with different volatile details must keep
		// deduping to one learned signature.
		if s.ErrorSummary != tc.wantSummary {
			t.Errorf("%s: ErrorSummary = %q, want %q", tc.name, s.ErrorSummary, tc.wantSummary)
		}
	}
}

func TestCodexRateLimitErrorCarriesApprovalLikeOptions(t *testing.T) {
	c := New(nil)
	data, err := os.ReadFile(filepath.Join("testdata", "transcripts", "error_codex_rate_limit.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"blocked", "idle", "done"} {
		s := c.Classify("codex", status, string(data))
		if s.Type != domain.SituationError || s.ErrorSummary != domain.CodexErrorRateLimit {
			t.Fatalf("rate-limit modal at %s = type %v summary %q", status, s.Type, s.ErrorSummary)
		}
		if len(s.Options) != 3 || !strings.HasPrefix(s.Options[0], "Switch to gpt-5.4-mini") || s.Options[1] != "Keep current model" {
			t.Fatalf("rate-limit options at %s = %v", status, s.Options)
		}
	}
	if other := c.Classify("claude", "blocked", string(data)); other.Type == domain.SituationError {
		t.Fatal("Codex rate-limit modal must remain scoped to codex")
	}
}

func TestGenericErrorNarrationNotError(t *testing.T) {
	// The generic error regexes were removed: a printed build failure /
	// stack trace (ordinary agent narration) must no longer read as a live
	// error situation.
	c := New(nil)
	pane := "$ go build ./...\nserver/handler.go:42: undefined: parseRequest\n" +
		"ERROR: build failed with exit code 1\npanic: nil pointer\n"
	if s := c.Classify("claude", "blocked", pane); s.Type == domain.SituationError {
		t.Errorf("narrated build failure must not classify error, got %v", s.Type)
	}
}

func TestClaudeMCQFormIsClaudeScoped(t *testing.T) {
	// The structural MCQ signal is claude-only: the identical form on another
	// agent type, even when blocked, must not classify as choice (guards the
	// agentType == "claude" gate at the choice position).
	c := New(nil)
	data, err := os.ReadFile(filepath.Join("testdata", "transcripts", "choice_claude_mcq_tabs_v2.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if s := c.Classify("codex", "blocked", string(data)); s.Type == domain.SituationChoice {
		t.Errorf("MCQ form on non-claude agent must not classify choice, got %v", s.Type)
	}
}

func TestApprovalWinsOverMCQFooter(t *testing.T) {
	// Priority invariant: a blocked claude pane that matches BOTH an approval
	// regex and the MCQ "Enter to select" footer must classify as approval —
	// the MCQ choice signal must never outrank approval.
	c := New(nil)
	pane := "Do you want to proceed with the edit?\n" +
		"❯ 1. Yes\n  2. No\n\nEnter to select · ↑/↓ to navigate · Esc to cancel\n"
	if s := c.Classify("claude", "blocked", pane); s.Type != domain.SituationApproval {
		t.Fatalf("approval must win over MCQ footer, got %v", s.Type)
	}
}

// Claude's plan-mode approval panel ("Claude has written up a plan and is
// ready to execute. Would you like to proceed?" with a numbered menu) asks
// with "would you like to", not "do you want to", so it previously fell
// through to unclassifiable (bare escalation). It must classify as approval
// with the four menu options extracted.
func TestPlanModeApprovalClassifiesApproval(t *testing.T) {
	c := New(nil)
	data, err := os.ReadFile(filepath.Join("testdata", "transcripts", "approval_claude_plan.txt"))
	if err != nil {
		t.Fatal(err)
	}
	s := c.Classify("claude", "blocked", string(data))
	if s.Type != domain.SituationApproval {
		t.Fatalf("plan-mode approval type = %v, want approval", s.Type)
	}
	if len(s.Options) != 4 {
		t.Errorf("plan-mode approval options = %d (%v), want 4", len(s.Options), s.Options)
	}
	if !strings.HasPrefix(s.PermissionVerb, "proceed") {
		t.Errorf("plan-mode approval verb = %q, want it to start with %q", s.PermissionVerb, "proceed")
	}
	// The same panel content when NOT blocked must not read as a live prompt.
	if s := c.Classify("claude", "working", string(data)); s.Type == domain.SituationApproval {
		t.Error("plan-mode panel at non-blocked status must not classify approval")
	}
}

func TestCodexPlanModeApprovalClassifiesApprovalAtParkedStatuses(t *testing.T) {
	c := New(nil)
	data, err := os.ReadFile(filepath.Join("testdata", "transcripts", "approval_codex_plan.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"blocked", "idle", "done"} {
		s := c.Classify("codex", status, string(data))
		if s.Type != domain.SituationApproval {
			t.Fatalf("Codex Plan approval at %s = %v, want approval", status, s.Type)
		}
		if s.PermissionVerb != "implement this plan" {
			t.Errorf("Codex Plan approval verb = %q", s.PermissionVerb)
		}
		if len(s.Options) != 3 {
			t.Fatalf("Codex Plan approval options = %d (%v), want 3", len(s.Options), s.Options)
		}
		if strings.Contains(strings.Join(s.Options, "|"), "This is plan content") {
			t.Errorf("plan body leaked into approval options: %v", s.Options)
		}
	}
	if s := c.Classify("codex", "working", string(data)); s.Type == domain.SituationApproval {
		t.Fatal("working Codex pane must not classify a Plan approval")
	}
	if s := c.Classify("claude", "idle", string(data)); s.Type == domain.SituationApproval {
		t.Fatal("Codex Plan approval structure must remain scoped to codex")
	}
}

func TestClaudeRemoteEnvClassifiesApprovalAtParkedStatuses(t *testing.T) {
	// Claude's "Select remote environment" picker (remote sub-agent launch)
	// stands while Herdr reports the pane idle, so it is a parked structural
	// approval — like Codex's Plan approval — at blocked/idle/done, never at
	// working, and scoped strictly to agent_type claude.
	c := New(nil)
	data, err := os.ReadFile(filepath.Join("testdata", "transcripts", "approval_claude_remote_env.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"blocked", "idle", "done"} {
		s := c.Classify("claude", status, string(data))
		if s.Type != domain.SituationApproval {
			t.Fatalf("remote-env picker at %s = %v, want approval", status, s.Type)
		}
		if s.PermissionVerb != "select remote environment" {
			t.Errorf("remote-env picker verb = %q", s.PermissionVerb)
		}
		if len(s.Options) != 4 {
			t.Fatalf("remote-env picker options = %d (%v), want 4", len(s.Options), s.Options)
		}
		// The default environment's ✔ marker is UI state, not part of the
		// label; a rule learned from the clean label must match either way.
		if s.Options[0] != "herdr-auto-pilot (env_01F41H1jxkGrT2zj55CqE4WQ)" {
			t.Errorf("remote-env picker option 1 = %q, want the ✔ stripped", s.Options[0])
		}
	}
	if s := c.Classify("claude", "working", string(data)); s.Type == domain.SituationApproval {
		t.Fatal("working claude pane must not classify the remote-env picker as approval")
	}
	if s := c.Classify("codex", "idle", string(data)); s.Type == domain.SituationApproval {
		t.Fatal("remote-env picker structure must remain scoped to claude")
	}
	// Its footer also matches the single-question MCQ shape; approval must
	// keep priority over choice at blocked status too.
	if s := c.Classify("claude", "blocked", string(data)); s.Type == domain.SituationChoice {
		t.Fatal("remote-env picker must classify approval, not choice")
	}
}

func TestApprovalFixturesStillApproval(t *testing.T) {
	// Regression: permission menus also render numbered options; approval
	// must keep winning (rule order encodes priority).
	c := New(nil)
	for _, name := range []string{"approval_permission.txt", "approval_yn.txt"} {
		data, err := os.ReadFile(filepath.Join("testdata", "transcripts", name))
		if err != nil {
			t.Fatal(err)
		}
		if s := c.Classify("claude", "blocked", string(data)); s.Type != domain.SituationApproval {
			t.Errorf("%s: type = %v, want approval", name, s.Type)
		}
	}
}

// TestCodexComposerStrippedOnlyForCodex proves the codex composer-line strip
// (domain.StripCodexComposer) is gated strictly on agent_type == "codex":
// claude's Content must come out byte-identical to the raw pane, while
// codex's Content has the "› ..." line gone and keeps the footer.
func TestCodexComposerStrippedOnlyForCodex(t *testing.T) {
	c := New(nil)
	panes := map[string]string{
		"absolute cwd": "─ Worked for 10m 49s ─────\n\n› Summarize recent commits\n\n  gpt-5.6-sol high · /workspaces/herdr-auto-pilot\n",
		// Issue #160: cwds under $HOME render ~-relative in the footer; the
		// strip must fire for those sessions too.
		"tilde cwd": "─ Worked for 10m 49s ─────\n\n› Use /skills to list available skills\n\n  gpt-5.6-sol high · ~/hap-codex-test\n",
	}
	for name, pane := range panes {
		t.Run(name, func(t *testing.T) {
			codex := c.Classify("codex", "idle", pane)
			if strings.Contains(codex.Content, "›") {
				t.Errorf("codex situation content must have composer line stripped, got %q", codex.Content)
			}
			if !strings.Contains(codex.Content, "gpt-5.6-sol high") {
				t.Errorf("codex situation content must keep the footer, got %q", codex.Content)
			}

			claude := c.Classify("claude", "idle", pane)
			if claude.Content != pane {
				t.Errorf("claude situation content must be untouched, got %q, want %q", claude.Content, pane)
			}

			if codex.Content == claude.Content {
				t.Error("expected codex and claude content to differ after gating")
			}
		})
	}
}
