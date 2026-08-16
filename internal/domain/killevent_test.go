package domain_test

import (
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// TestKillEventLabel pins the operator-facing spelling of every scope/state
// pair the automation history can hold, plus the pass-through for pairs it does
// not know: a row written by a newer build must stay readable rather than
// rendering blank or being relabelled as something it is not.
//
// The cross-scope rows are the point. No writer produces them today, but a
// state read WITHOUT its scope would render an "active" full self-prompting row
// as "paused" — a mode toggle wearing the kill switch's label in the one list
// that shows both streams.
func TestKillEventLabel(t *testing.T) {
	for _, tc := range []struct {
		scope string
		state string
		want  string
	}{
		{domain.KillScopeGlobal, domain.KillStateActiveValue, "paused"},
		{domain.KillScopeGlobal, domain.KillStateResumed, "resumed"},
		{domain.KillScopeFSP, domain.KillStateFSPOn, "FSP On"},
		{domain.KillScopeFSP, domain.KillStateFSPOff, "FSP Off"},
		// An empty scope reads as global: a hand-built event in a test or a
		// fake carries none, while SQLite hands back the column default.
		{"", domain.KillStateActiveValue, "paused"},
		{"", domain.KillStateResumed, "resumed"},
		// Cross-scope: never borrow the other stream's label.
		{domain.KillScopeFSP, domain.KillStateActiveValue, domain.KillStateActiveValue},
		{domain.KillScopeGlobal, domain.KillStateFSPOn, domain.KillStateFSPOn},
		// Unknown scope and unknown state both pass through.
		{"something_new", domain.KillStateActiveValue, domain.KillStateActiveValue},
		{domain.KillScopeGlobal, "something_new", "something_new"},
		{"", "", ""},
	} {
		got := domain.KillEventLabel(domain.KillEvent{Scope: tc.scope, State: tc.state})
		if got != tc.want {
			t.Errorf("KillEventLabel(scope=%q, state=%q) = %q, want %q",
				tc.scope, tc.state, got, tc.want)
		}
	}
}

// TestKillStateActiveOnlyMatchesTheActiveState guards the halt predicate: every
// other state — including the two full self-prompting ones that now share the
// table — must read as "not halted", so a mis-scoped row can never be mistaken
// for a pause.
func TestKillStateActiveOnlyMatchesTheActiveState(t *testing.T) {
	if !domain.KillStateActive(&domain.KillEvent{State: domain.KillStateActiveValue}) {
		t.Error("the active state stopped halting automation")
	}
	for _, state := range []string{
		domain.KillStateResumed, domain.KillStateFSPOn, domain.KillStateFSPOff, "",
	} {
		if domain.KillStateActive(&domain.KillEvent{State: state}) {
			t.Errorf("state %q reads as a halted kill switch", state)
		}
	}
	if domain.KillStateActive(nil) {
		t.Error("a database with no kill events reads as paused")
	}
}
