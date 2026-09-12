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
	if hit, matched := list.Match("agy", agyApproval); !matched {
		_ = hit
		// Matching the whole pane text is not itself wrong (it contains the
		// answer); what matters is that this list is never HANDED pane text.
		// Documented here so the call-site test is understood as the real guard.
		t.Log("note: the list is content-blind; daemon.actionRefused is only ever called with a chosen action")
	}
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
	list, _ := NewNeverAutoActionList(true, []string{persist}, nil)
	if _, matched := list.Match("agy", "Yes, and always allow (Persist to settings.json)"); !matched {
		t.Error("control: the other seeds must still fire on this answer")
	}
	only, _ := NewNeverAutoActionList(true, []string{persist}, nil)
	if _, matched := only.Match("agy", "write it to the Persist to settings.json file"); matched {
		t.Error("a silenced seed must not fire")
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
