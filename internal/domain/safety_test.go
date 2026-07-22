package domain

import (
	"bufio"
	"os"
	"strings"
	"testing"
	"time"
)

func newSeedNeverAuto(t *testing.T) *NeverAutoList {
	t.Helper()
	a, errs := NewNeverAutoList(true, nil, nil)
	if len(errs) > 0 {
		t.Fatalf("seed allowlist failed to compile: %v", errs)
	}
	return a
}

// TestSeedNeverAutoCatchesCorpus is the CI regression gate (NFR-005a):
// 100% of the irreversible-op corpus must be matched by seed patterns.
// A corpus miss fails the build.
func TestSeedNeverAutoCatchesCorpus(t *testing.T) {
	a := newSeedNeverAuto(t)

	f, err := os.Open("testdata/irreversible_corpus.txt")
	if err != nil {
		t.Fatalf("open corpus: %v", err)
	}
	defer f.Close()

	var total, missed int
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		total++
		if _, ok := a.Match("claude", line); !ok {
			missed++
			t.Errorf("corpus entry NOT matched by seed allowlist: %q", line)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	if total < 40 {
		t.Fatalf("corpus suspiciously small (%d entries); maintenance regression?", total)
	}
	t.Logf("allowlist corpus: %d/%d matched", total-missed, total)
}

func TestNeverAutoDoesNotMatchBenignPrompts(t *testing.T) {
	a := newSeedNeverAuto(t)
	benign := []string{
		"Do you want to run the unit test suite now?",
		"Allow reading the file src/main.go?",
		"Run go build ./... to check compilation?",
		"Should I add a new test for the parser?",
		"Apply the suggested refactor to the config loader?",
		"Run git status and show the diff?",
		"Commit the staged changes with message 'fix: handle nil input'?",
		"Truncate the log line to 80 characters?",
	}
	for _, p := range benign {
		if hit, ok := a.Match("claude", p); ok {
			t.Errorf("benign prompt matched allowlist pattern %q: %q", hit.Pattern, p)
		}
	}
}

func TestOperatorPatternsExtendSeed(t *testing.T) {
	a, errs := NewNeverAutoList(true, []string{`(?i)restart\s+the\s+payment\s+service`}, nil)
	if len(errs) > 0 {
		t.Fatalf("compile: %v", errs)
	}
	hit, ok := a.Match("claude", "Please restart the payment service now")
	if !ok {
		t.Error("operator-added pattern should match")
	} else if hit.Excerpt != "restart the payment service" || hit.Source != NeverAutoOperator || hit.Kind != NeverAutoStrict {
		t.Errorf("operator hit diagnostics incomplete: %+v", hit)
	} else {
		diagnostic := hit.Diagnostic()
		if !strings.Contains(diagnostic, hit.Pattern) || !strings.Contains(diagnostic, `matched "restart the payment service"`) ||
			!strings.Contains(diagnostic, "source=operator") || !strings.Contains(diagnostic, "kind=strict") {
			t.Errorf("formatted diagnostic missing pattern, excerpt, or metadata: %q", diagnostic)
		}
	}
}

func TestDisableSeedRemovesStrictAndHeuristicRules(t *testing.T) {
	a, errs := NewNeverAutoList(false, []string{`operator-only`}, nil)
	if len(errs) > 0 {
		t.Fatalf("compile: %v", errs)
	}
	for _, rule := range a.Rules() {
		if rule.Source == NeverAutoSeed {
			t.Fatalf("disabled matcher retained shipped rule: %+v", rule)
		}
	}
	if _, ok := a.Match("claude", "git push --force origin main"); ok {
		t.Error("strict seed pattern must be disabled")
	}
	if _, ok := a.SuspectedIrreversible("claude", "This action cannot be undone"); ok {
		t.Error("heuristic seed rule must be disabled")
	}
	if _, ok := a.Match("claude", "operator-only"); !ok {
		t.Error("disabling shipped rules must not disable operator rules")
	}
}

func TestNeverAutoStrictAndHeuristicRegressionMatrix(t *testing.T) {
	operatorPattern := `(?i)restart\s+the\s+payments\s+service`
	scopedPattern := `(?i)compact\s+the\s+conversation`
	operatorRules := []NeverAutoRule{{
		Pattern: scopedPattern, AgentTypes: []string{"codex"},
	}}
	enabled, errs := NewNeverAutoList(true, []string{operatorPattern}, operatorRules)
	if len(errs) > 0 {
		t.Fatalf("compile enabled matcher: %v", errs)
	}
	disabled, errs := NewNeverAutoList(false, []string{operatorPattern}, operatorRules)
	if len(errs) > 0 {
		t.Fatalf("compile disabled matcher: %v", errs)
	}

	type matchKind string
	const (
		strictMatch    matchKind = "strict"
		heuristicMatch matchKind = "heuristic"
	)
	tests := []struct {
		name        string
		matcher     *NeverAutoList
		matchKind   matchKind
		agentType   string
		content     string
		want        bool
		wantSource  NeverAutoRuleSource
		wantPattern string
		wantExcerpt string
	}{
		{
			name: "shipped strict rule", matcher: enabled, matchKind: strictMatch,
			agentType: "claude", content: "Run git reset --hard now?", want: true,
			wantSource: NeverAutoSeed, wantPattern: `(?i)git\s+reset\s+--hard`, wantExcerpt: "git reset --hard",
		},
		{
			name: "strict rule does not become heuristic", matcher: enabled, matchKind: heuristicMatch,
			agentType: "claude", content: "Run git reset --hard now?", want: false,
		},
		{
			name: "shipped heuristic rule", matcher: enabled, matchKind: heuristicMatch,
			agentType: "claude", content: "This action cannot be undone. Continue?", want: true,
			wantSource: NeverAutoSeed, wantExcerpt: "cannot be undone",
		},
		{
			name: "heuristic rule does not become strict", matcher: enabled, matchKind: strictMatch,
			agentType: "claude", content: "This action cannot be undone. Continue?", want: false,
		},
		{
			name: "operator regex is strict", matcher: enabled, matchKind: strictMatch,
			agentType: "claude", content: "Please RESTART   the payments service.", want: true,
			wantSource: NeverAutoOperator, wantPattern: operatorPattern, wantExcerpt: "RESTART the payments service",
		},
		{
			name: "scoped operator rule matches agent case insensitively", matcher: enabled, matchKind: strictMatch,
			agentType: "CODEX", content: "Compact the conversation now.", want: true,
			wantSource: NeverAutoOperator, wantPattern: scopedPattern, wantExcerpt: "Compact the conversation",
		},
		{
			name: "scoped operator rule rejects another agent", matcher: enabled, matchKind: strictMatch,
			agentType: "claude", content: "Compact the conversation now.", want: false,
		},
		{
			name: "seed disabling removes strict rule", matcher: disabled, matchKind: strictMatch,
			agentType: "claude", content: "Run git reset --hard now?", want: false,
		},
		{
			name: "seed disabling removes heuristic rule", matcher: disabled, matchKind: heuristicMatch,
			agentType: "claude", content: "This action cannot be undone. Continue?", want: false,
		},
		{
			name: "seed disabling preserves operator regex", matcher: disabled, matchKind: strictMatch,
			agentType: "claude", content: "Restart the payments service.", want: true,
			wantSource: NeverAutoOperator, wantPattern: operatorPattern, wantExcerpt: "Restart the payments service",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var hit NeverAutoHit
			var ok bool
			if tc.matchKind == strictMatch {
				hit, ok = tc.matcher.Match(tc.agentType, tc.content)
			} else {
				hit, ok = tc.matcher.SuspectedIrreversible(tc.agentType, tc.content)
			}
			if ok != tc.want {
				t.Fatalf("matched = %v, want %v (hit %+v)", ok, tc.want, hit)
			}
			if !tc.want {
				return
			}
			wantRuleKind := NeverAutoStrict
			if tc.matchKind == heuristicMatch {
				wantRuleKind = NeverAutoHeuristic
			}
			if hit.Source != tc.wantSource || hit.Kind != wantRuleKind {
				t.Errorf("metadata = source %q kind %q, want source %q kind %q",
					hit.Source, hit.Kind, tc.wantSource, wantRuleKind)
			}
			if tc.wantPattern != "" && hit.Pattern != tc.wantPattern {
				t.Errorf("pattern = %q, want %q", hit.Pattern, tc.wantPattern)
			}
			if hit.Pattern == "" {
				t.Error("matched rule must report its pattern")
			}
			if hit.Excerpt != tc.wantExcerpt {
				t.Errorf("excerpt = %q, want %q", hit.Excerpt, tc.wantExcerpt)
			}
		})
	}
}

func TestUnifiedNeverAutoRulesPreserveMetadataAndScope(t *testing.T) {
	scopedPattern := `(?i)compact\s+the\s+conversation`
	a, errs := NewNeverAutoList(true, []string{`operator-global`}, []NeverAutoRule{{
		Pattern: scopedPattern, AgentTypes: []string{"codex", "agy"},
	}})
	if len(errs) > 0 {
		t.Fatalf("compile: %v", errs)
	}
	if _, ok := a.Match("codex", "Compact the conversation now"); !ok {
		t.Error("scoped strict rule must match a listed agent type")
	}
	if _, ok := a.Match("claude", "Compact the conversation now"); ok {
		t.Error("scoped strict rule must not match an unlisted agent type")
	}

	var seedStrict, seedHeuristic, operatorGlobal, operatorScoped bool
	for _, rule := range a.Rules() {
		switch {
		case rule.Source == NeverAutoSeed && rule.Kind == NeverAutoStrict:
			seedStrict = true
		case rule.Source == NeverAutoSeed && rule.Kind == NeverAutoHeuristic:
			seedHeuristic = true
		case rule.Pattern == "operator-global" && rule.Source == NeverAutoOperator && rule.Kind == NeverAutoStrict:
			operatorGlobal = true
		case rule.Pattern == scopedPattern && rule.Source == NeverAutoOperator && rule.Kind == NeverAutoStrict && len(rule.AgentTypes) == 2:
			operatorScoped = true
		}
	}
	if !seedStrict || !seedHeuristic || !operatorGlobal || !operatorScoped {
		t.Fatalf("unified rule metadata incomplete: seedStrict=%v seedHeuristic=%v operatorGlobal=%v operatorScoped=%v",
			seedStrict, seedHeuristic, operatorGlobal, operatorScoped)
	}
}

func TestInvalidOperatorPatternReported(t *testing.T) {
	_, errs := NewNeverAutoList(true, []string{`([unclosed`}, nil)
	if len(errs) == 0 {
		t.Error("invalid pattern must be reported")
	}
}

func TestSuspectedIrreversibleHeuristic(t *testing.T) {
	a := newSeedNeverAuto(t)
	suspicious := []string{
		"This will permanently erase the workspace metadata. Continue?",
		"Overwrite the existing changes and discard local work?",
		"This action cannot be undone. Proceed?",
		"Delete all rows from the users table?",
		"This wipes the database and restores factory defaults.",
		"Revoke the API tokens for the staging tenant?",
		"Are you sure you want to delete these branches?",
		"Are you absolutely sure?",
		"Force overwrite the remote copy?",
		"Publish the package to the public registry?",
		"Removing the backups frees 2GB. Continue?",
		"This wipes the databases for every tenant.",
		"Drop all tables and re-run the migration?",
		"Delete the user accounts flagged as spam?",
		"This change is permanent and cannot be reversed.",
		"Delete the following?\n  - production database backups",
	}
	for _, p := range suspicious {
		if _, ok := a.SuspectedIrreversible("claude", p); !ok {
			t.Errorf("expected suspected-irreversible for %q", p)
		}
	}

	// Everyday coding prompts contain destructive-sounding verbs; bare
	// verbs without a destructive target must NOT trip the heuristic
	// (operator-reported false alarms).
	benign := []string{
		"Run the formatter on the changed files?",
		"Do you want to remove the unused import?",
		"Drop the extra parameter from the function signature?",
		"Delete the commented-out block in parser.go?",
		"Rotate the image 90 degrees before saving the thumbnail?",
		"Force the type assertion here instead of reflection?",
		"Should I push the branch and open a pull request?",
		"Are you sure you want to continue?",
		"Purge the memoization cache entry after each test?",
		"Truncate the log line to 80 characters?",
		"Erase the whiteboard diagram from the README?",
	}
	for _, p := range benign {
		if hit, ok := a.SuspectedIrreversible("claude", p); ok {
			t.Errorf("benign prompt should not trip the heuristic: %q (indicator %q matched %q)",
				p, hit.Pattern, hit.Excerpt)
		}
	}
}

func TestSuspectedIrreversibleReportsHit(t *testing.T) {
	a := newSeedNeverAuto(t)
	hit, ok := a.SuspectedIrreversible("claude", "This wipes   the\ndatabase for every tenant.")
	if !ok {
		t.Fatal("expected a heuristic hit")
	}
	if hit.Pattern == "" {
		t.Error("hit must carry the matching indicator pattern")
	}
	if hit.Excerpt != "wipes the database" {
		t.Errorf("excerpt should be the whitespace-collapsed match, got %q", hit.Excerpt)
	}
}

func TestSuspectedIrreversibleIgnoresDistantNarration(t *testing.T) {
	// Regression for the agy false-positive: a destructive verb and a data
	// target separated by more than one line break is conversation about an
	// operation, not a pending one.
	a := newSeedNeverAuto(t)
	narration := "The summarizer described deleting entries yesterday.\n" +
		"That indicator needs corroboration to fire.\n" +
		"All databases stay healthy and untouched."
	if hit, ok := a.SuspectedIrreversible("agy", narration); ok {
		t.Errorf("distant verb/target must not trip the heuristic (indicator %q matched %q)",
			hit.Pattern, hit.Excerpt)
	}
	// Adjacent lines still count: confirmations wrap.
	confirm := "Delete the following?\n  - production database backups"
	if _, ok := a.SuspectedIrreversible("agy", confirm); !ok {
		t.Error("verb and target on adjacent lines must still trip the heuristic")
	}
	// A single blank line between verb and target is still dialog formatting.
	blankLine := "The following will be deleted:\n\n  - customer data tables"
	if _, ok := a.SuspectedIrreversible("agy", blankLine); !ok {
		t.Error("verb and target separated by one blank line must still trip the heuristic")
	}
}

func TestEmptyMatchableIndicatorStillFires(t *testing.T) {
	// A misconfigured operator pattern that can match the empty string must
	// fire (noisy-safe), not silently disable itself.
	a, errs := NewNeverAutoList(false, nil, []NeverAutoRule{{
		Pattern: `(?i)(drop prod)?`, Kind: NeverAutoHeuristic, Source: NeverAutoOperator,
	}})
	if len(errs) > 0 {
		t.Fatalf("compile: %v", errs)
	}
	if _, ok := a.SuspectedIrreversible("claude", "anything at all"); !ok {
		t.Error("empty-matchable indicator must still fire")
	}
}

func TestAgentScopedIndicators(t *testing.T) {
	rules := []NeverAutoRule{
		{Pattern: `(?i)compact\s+the\s+conversation`, AgentTypes: []string{"codex", "agy"}, Kind: NeverAutoHeuristic},
		{Pattern: `(?i)squash\s+the\s+timeline`, AgentTypes: []string{"*"}, Kind: NeverAutoHeuristic},
	}
	a, errs := NewNeverAutoList(false, nil, rules)
	if len(errs) > 0 {
		t.Fatalf("compile: %v", errs)
	}

	scoped := "Compact the conversation history now?"
	if _, ok := a.SuspectedIrreversible("agy", scoped); !ok {
		t.Error("scoped indicator must apply to a listed agent")
	}
	if _, ok := a.SuspectedIrreversible("AGY", scoped); !ok {
		t.Error("agent matching must be case-insensitive")
	}
	if _, ok := a.SuspectedIrreversible("claude", scoped); ok {
		t.Error("scoped indicator must not apply to unlisted agents")
	}

	// Sloppy scope entries fail noisy, not silently dead: a padded "*" is
	// still a wildcard and a blank entry is treated as one.
	padded, errs := NewNeverAutoList(false, nil, []NeverAutoRule{
		{Pattern: `(?i)compact\s+the\s+conversation`, AgentTypes: []string{" * "}, Kind: NeverAutoHeuristic},
		{Pattern: `(?i)squash\s+the\s+timeline`, AgentTypes: []string{""}, Kind: NeverAutoHeuristic},
	})
	if len(errs) > 0 {
		t.Fatalf("compile: %v", errs)
	}
	if _, ok := padded.SuspectedIrreversible("claude", scoped); !ok {
		t.Error("padded \"*\" entry must act as a wildcard")
	}
	if _, ok := padded.SuspectedIrreversible("claude", "Squash the timeline?"); !ok {
		t.Error("blank agents entry must act as a wildcard")
	}

	wildcard := "Squash the timeline?"
	for _, agent := range []string{"claude", "codex", "agy", ""} {
		if _, ok := a.SuspectedIrreversible(agent, wildcard); !ok {
			t.Errorf("wildcard indicator must apply to agent %q", agent)
		}
	}
}

func TestSeedIndicatorsApplyToAllAgents(t *testing.T) {
	a := newSeedNeverAuto(t)
	for _, agent := range []string{"claude", "codex", "agy", "unknown", ""} {
		if _, ok := a.SuspectedIrreversible(agent, "This action cannot be undone."); !ok {
			t.Errorf("seed indicators must apply to agent %q", agent)
		}
	}
}

func TestIrreversibleScanContent(t *testing.T) {
	// Idle: only the outbound next-task text is scanned — stale narration in
	// the pane must not leak into the heuristic.
	idle := Situation{Type: SituationIdle,
		Content: "I finished. Earlier we discussed deleting\ndatabases in the design doc.\nTask complete."}
	got := IrreversibleScanContent(idle, "run the linter")
	if got != "run the linter" {
		t.Errorf("idle scan must be the declared task only, got %q", got)
	}

	// Idle with the agent's native todo widget: the inferred next item is
	// scanned (inference is per-agent-type, so AgentType must be set).
	idleList := Situation{Type: SituationIdle, AgentType: "claude",
		Content: "  ⎿  ✔ step one\n     □ drop the users table"}
	got = IrreversibleScanContent(idleList, "")
	if !strings.Contains(got, "drop the users table") {
		t.Errorf("idle scan must include the inferred next task, got %q", got)
	}

	// Approval: the tail window plus extracted fields — narration beyond the
	// window is excluded, the pending dialog is included.
	var b strings.Builder
	b.WriteString("narration: deleting\ndatabases was discussed here\n")
	for i := 0; i < IrreversibleScanTailLines; i++ {
		b.WriteString("filler line\n")
	}
	b.WriteString("Allow dropping the orders table?\n1. Yes\n2. No")
	approval := Situation{Type: SituationApproval, Content: b.String(),
		Options: []string{"Yes", "No"}, PermissionVerb: "proceed"}
	got = IrreversibleScanContent(approval, "unused for approvals")
	if strings.Contains(got, "narration") {
		t.Errorf("approval scan must exclude content above the tail window, got %q", got)
	}
	if !strings.Contains(got, "Allow dropping the orders table?") {
		t.Errorf("approval scan must include the pending dialog, got %q", got)
	}
	if !strings.Contains(got, "proceed") || !strings.Contains(got, "Yes") {
		t.Errorf("approval scan must include extracted fields, got %q", got)
	}
	if strings.Contains(got, "unused for approvals") {
		t.Errorf("approval scan must not include the declared task, got %q", got)
	}

	// Choice and error situations include their extracted fields.
	choice := Situation{Type: SituationChoice, Content: "Pick one:\n1. keep\n2. drop schema",
		Options: []string{"keep", "drop schema"}}
	got = IrreversibleScanContent(choice, "")
	if !strings.Contains(got, "drop schema") {
		t.Errorf("choice scan must include the options, got %q", got)
	}
	errSit := Situation{Type: SituationError, Content: "Error: task failed",
		ErrorSummary: "task failed irrecoverably"}
	got = IrreversibleScanContent(errSit, "")
	if !strings.Contains(got, "task failed irrecoverably") {
		t.Errorf("error scan must include the error summary, got %q", got)
	}

	// Fail-safe default: any other situation type scans the full content.
	unclassifiable := Situation{Type: SituationUnclassifiable, Content: "entire\npane\ncontent"}
	if got := IrreversibleScanContent(unclassifiable, ""); got != "entire\npane\ncontent" {
		t.Errorf("unclassifiable scan must be the full content, got %q", got)
	}
}

func TestRateGuardFunctions(t *testing.T) {
	now := time.Now()
	lim := RateLimits{MaxConsecutive: 5, MaxPerMinute: 10}

	r := AgentRate{}
	for i := 0; i < 5; i++ {
		ok, _ := CheckRate(r, now, lim)
		if !ok {
			t.Fatalf("prompt %d should be allowed", i+1)
		}
		r = RegisterAutoPrompt(r, now)
	}
	if ok, reason := CheckRate(r, now, lim); ok || reason != ReasonRateLimited {
		t.Error("6th consecutive prompt must be blocked")
	}

	// Human interaction resets the consecutive counter.
	r = RegisterHumanInteraction(r)
	if ok, _ := CheckRate(r, now, lim); !ok {
		t.Error("automation should resume after human interaction")
	}

	// Per-minute window.
	r = AgentRate{WindowStart: now.Add(-10 * time.Second), CountInWindow: 10}
	if ok, _ := CheckRate(r, now, lim); ok {
		t.Error("11th prompt within the minute must be blocked")
	}
	// Window expiry allows again (consecutive still under ceiling).
	r = AgentRate{WindowStart: now.Add(-2 * time.Minute), CountInWindow: 10}
	if ok, _ := CheckRate(r, now, lim); !ok {
		t.Error("expired window should allow prompting again")
	}

	// Paused agents stay blocked until human interaction.
	r = PauseAgent(AgentRate{})
	if ok, _ := CheckRate(r, now, lim); ok {
		t.Error("paused agent must stay blocked")
	}
}

// TestRegisterAutoPromptIdleSkipsConsecutiveCounter pins the auto-send-when-idle
// exemption: an unattended idle task hand-out must NOT advance the consecutive
// counter (or the source would pause after max_consecutive_auto_prompts tasks),
// but it MUST still advance the per-minute window so a runaway source is
// throttled.
func TestRegisterAutoPromptIdleSkipsConsecutiveCounter(t *testing.T) {
	now := time.Now()
	lim := RateLimits{MaxConsecutive: 5, MaxPerMinute: 100}

	// Far more idle hand-outs than the consecutive ceiling never trips it.
	r := AgentRate{WindowStart: now}
	for range lim.MaxConsecutive * 3 {
		r = RegisterAutoPromptIdle(r, now)
	}
	if r.ConsecutiveAuto != 0 {
		t.Errorf("idle hand-outs must not advance ConsecutiveAuto, got %d", r.ConsecutiveAuto)
	}
	if r.CountInWindow != lim.MaxConsecutive*3 {
		t.Errorf("idle hand-outs must advance the per-minute window, got %d want %d",
			r.CountInWindow, lim.MaxConsecutive*3)
	}
	if ok, _ := CheckRate(r, now, lim); !ok {
		t.Error("idle hand-outs alone must never trip the consecutive ceiling")
	}

	// The per-minute cap still throttles idle hand-outs.
	perMin := RateLimits{MaxConsecutive: 100, MaxPerMinute: 3}
	r = AgentRate{WindowStart: now}
	for i := range perMin.MaxPerMinute {
		if ok, _ := CheckRate(r, now, perMin); !ok {
			t.Fatalf("idle hand-out %d should be allowed within the minute", i+1)
		}
		r = RegisterAutoPromptIdle(r, now)
	}
	if ok, reason := CheckRate(r, now, perMin); ok || reason != ReasonRateLimited {
		t.Error("idle hand-out over the per-minute cap must be blocked")
	}
}
