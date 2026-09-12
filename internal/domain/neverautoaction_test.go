package domain

import "testing"

// agyApproval is the shape that made the situation-side rules unusable for this
// job: the dangerous options are printed on EVERY approval, so a rule matched
// against the screen fires every time.
const agyApproval = "Requesting permission for:\n   go test ./...\n\nRun this command?\n" +
	"> 1. Yes, run command\n" +
	"  2. Yes, and always allow in this conversation for commands that start with 'go'\n" +
	"  3. Yes, and always allow for commands that start with 'go' (Persist to settings.json)\n" +
	"  4. No, cancel\n"

// THE BUG THIS KEY EXISTS FOR.
//
// An action rule must judge the ANSWER alone. The orchestrator added
// `(?i)[Pp]ersist to settings\.json` as an ordinary never-auto rule to avoid
// ever PICKING that option; because that row is printed on every agy approval
// the rule matched every prompt, escalated all of them, and the agent could not
// progress (2026-09-12, removed four minutes later).
//
// Without the second assertion here a test suite passes whether the feature
// wired one list or two — it is the whole difference.
func TestNeverAutoActionRuleJudgesTheAnswerNotTheScreen(t *testing.T) {
	list, errs := NewNeverAutoActionList(false, nil, []NeverAutoRule{
		{Pattern: `(?i)persist to settings\.json`},
	})
	if len(errs) != 0 {
		t.Fatalf("compile errors: %v", errs)
	}
	if _, matched := list.Match("agy", "Yes, and always allow for commands that start with 'go' (Persist to settings.json)"); !matched {
		t.Error("the answer that picks the persisting option must be refused")
	}
	// The control: the SAME text appears in the pane, and the action list must
	// not care. Situation screening is a different list at different call sites.
	if _, matched := list.Match("agy", "Yes, run command"); matched {
		t.Error("the narrow answer must stay available — refusing it is what stalled the agent")
	}
	// Deliberately NOT asserted here, either way: handed the whole screen this
	// list matches it, because the screen CONTAINS the answer — the list is
	// content-blind and cannot tell the two apart. What makes the feature work
	// is that it is never handed pane text, which only a call-site test can
	// prove (daemon.actionRefused, and TestAutoAcceptScreensTheAnswerItWouldSend
	// for the unattended path). A branch here that logged on one outcome and
	// said nothing on the other asserted neither.
	_ = agyApproval
}

func TestNeverAutoActionRulesHonourAgentScope(t *testing.T) {
	list, _ := NewNeverAutoActionList(false, nil, []NeverAutoRule{
		{Pattern: `(?i)always allow`, AgentTypes: []string{"agy"}},
	})
	if _, matched := list.Match("agy", "Yes, and always allow"); !matched {
		t.Error("the scoped agent must be covered")
	}
	if _, matched := list.Match("claude", "Yes, and always allow"); matched {
		t.Error("a scoped rule must not apply to another agent type")
	}
}

// The seeds ship OFF, and that is load-bearing rather than incidental: refusing
// an option only escalates it, so enabling them before hap prefers the narrowest
// option turns most approvals into escalations — and ReasonNeverAutoMatch is in
// autoAcceptExcludedReasons, making each one permanently operator-only.
func TestNeverAutoActionSeedsAreOffUntilEnabled(t *testing.T) {
	widening := "Yes, and always allow in this conversation for commands that start with 'go'"

	off, _ := NewNeverAutoActionList(false, nil, nil)
	if _, matched := off.Match("agy", widening); matched {
		t.Error("the shipped action seeds must be inert until the operator opts in")
	}

	on, _ := NewNeverAutoActionList(true, nil, nil)
	if _, matched := on.Match("agy", widening); !matched {
		t.Error("enabled seeds must refuse a scope-widening option")
	}
	// The narrow answer must survive the seeds, or enabling them leaves hap with
	// no acceptable answer at all.
	if _, matched := on.Match("agy", "Yes, run command"); matched {
		t.Error("the seeds must leave the narrowest option answerable")
	}
	if _, matched := on.Match("agy", "No, cancel"); matched {
		t.Error("declining must stay answerable")
	}
}

func TestNeverAutoActionSeedsCanBeSilencedIndividually(t *testing.T) {
	var persist string
	for _, r := range SeedNeverAutoActionRules {
		if r.Pattern == `(?i)\bpersist\s+to\s+settings\.json\b` {
			persist = r.Pattern
		}
	}
	if persist == "" {
		t.Fatal("the settings.json seed rule is missing")
	}
	// The discriminator is the TEXT: of the four shipped action seeds, only the
	// persist rule matches this answer — it carries no "and always allow", no
	// "don't ask again" and no "allow all". An answer several seeds match
	// cannot discriminate at all, because the pair below would pass just as
	// happily if the disable had dropped the WRONG rule, or every rule but it.
	const onlyPersistMatches = "Yes, allow this command (Persist to settings.json)"

	enabled, _ := NewNeverAutoActionList(true, nil, nil)
	if _, matched := enabled.Match("agy", onlyPersistMatches); !matched {
		t.Fatal("control: with the seeds on, the persist rule must refuse this answer — " +
			"without this half, the assertion below passes on a list that refuses nothing")
	}
	silenced, _ := NewNeverAutoActionList(true, []string{persist}, nil)
	if _, matched := silenced.Match("agy", onlyPersistMatches); matched {
		t.Error("a silenced seed must not fire")
	}
	// Silencing ONE seed leaves the rest armed, which is what makes this a
	// per-rule switch rather than enable_never_auto_action_seeds by another name.
	if _, matched := silenced.Match("agy", "Yes, and always allow in this conversation"); !matched {
		t.Error("silencing the persist seed disarmed the other action seeds too")
	}
	// An operator rule is never filtered by disabled_seed_patterns, even when
	// its pattern is byte-identical to a silenced seed: the operator wrote it
	// after silencing the builtin, so it is the newer instruction.
	withOperator, _ := NewNeverAutoActionList(true, []string{persist}, []NeverAutoRule{{Pattern: persist}})
	if _, matched := withOperator.Match("agy", onlyPersistMatches); !matched {
		t.Error("an operator rule must survive a disabled_seed_patterns entry naming the same pattern")
	}
}

// Every shipped action seed must be addressable by the same id and disable
// commands the situation seeds are. They read the same
// safety.disabled_seed_patterns, so the domain side always supported silencing
// one — but every resolver searched the SITUATION set alone, so `rules list`
// printed an id and a [disabled] column for rules no command could resolve:
// `hap config rules disable-seed <that id>` failed with `no seed rule`, and the
// column could never become true.
func TestEveryShippedActionSeedIsAddressableByID(t *testing.T) {
	for _, r := range SeedNeverAutoActionRules {
		id := SeedRuleID(r.Pattern)
		got, ok := SeedRuleByID(id)
		if !ok {
			t.Errorf("SeedRuleByID(%q) found nothing for action seed %q", id, r.Pattern)
			continue
		}
		if got.Pattern != r.Pattern {
			t.Errorf("SeedRuleByID(%q) = %q, want %q", id, got.Pattern, r.Pattern)
		}
		if !IsSeedPattern(r.Pattern) {
			t.Errorf("IsSeedPattern(%q) = false, so App.DisableSeedRule refuses to record it",
				r.Pattern)
		}
	}
	// The union must not leak the other way: the compiled SITUATION seed set is
	// what carries the irreversibility heuristic, and an action rule reaching it
	// would judge a chosen menu option against indicators written for pane
	// content.
	for _, r := range SeedNeverAutoRules() {
		for _, a := range SeedNeverAutoActionRules {
			if r.Pattern == a.Pattern {
				t.Errorf("action seed %q is also in the situation seed set", a.Pattern)
			}
		}
	}
}

// An escalation an ACTION seed forced must offer the same "silence this builtin
// rule" hint a situation seed's does. It carries the same [never_auto_match]
// reason and the same diagnostic shape, so searching one side left exactly the
// rows that most need the hint — the rule refused an ANSWER, so the operator
// cannot rephrase their way past it — with none at all.
func TestAnActionSeedEscalationNamesTheRuleThatForcedIt(t *testing.T) {
	for _, r := range SeedNeverAutoActionRules {
		hit := NeverAutoHit{Pattern: r.Pattern, Source: NeverAutoSeed,
			Kind: NeverAutoStrict, Excerpt: "Yes, and always allow"}
		rationale := "[" + string(ReasonNeverAutoMatch) + "] " + hit.Diagnostic()
		got, ok := SeedRuleForcedEscalation(rationale)
		if !ok {
			t.Errorf("no seed rule attributed for action rule %q; the operator is offered no way to silence it",
				r.Pattern)
			continue
		}
		if got.Pattern != r.Pattern {
			t.Errorf("attributed %q, want %q", got.Pattern, r.Pattern)
		}
	}
}

// Every action rule is strict. The matcher underneath also carries the
// irreversibility heuristic, and a rule defaulted into it would judge action
// text against indicators written for pane content.
func TestNeverAutoActionRulesAreAlwaysStrict(t *testing.T) {
	list, _ := NewNeverAutoActionList(true, nil, []NeverAutoRule{
		{Pattern: `(?i)wipe everything`, Kind: NeverAutoHeuristic},
	})
	for _, r := range list.Rules() {
		if r.Kind != NeverAutoStrict {
			t.Errorf("rule %q has kind %q, want strict", r.Pattern, r.Kind)
		}
	}
}
