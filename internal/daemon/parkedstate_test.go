package daemon

import (
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// agy reports a parked agent as "done" where claude reports "idle", and the
// original write-up claimed hap therefore misses a parked agy agent. Reading
// the code says otherwise: every gate below already accepts both spellings.
//
// What was missing is a test that says so. Each of these predicates is a
// separate hand-written switch, so "done" can be dropped from any ONE of them
// without failing anything else — and the failure that produces is silent, an
// agy agent that is simply never considered parked by that one path.
//
// These are driven DIRECTLY rather than through the pipeline on purpose:
// end-to-end coverage of one path proves nothing about the other three.

func TestParkedStatesAcceptDoneAsWellAsIdle(t *testing.T) {
	// The auto-accept pass also treats a blocked agent as parked (a standing
	// modal is exactly what it exists to answer); the other two do not.
	cases := []struct {
		status                            string
		autoSend, sessionRename, autoAcpt bool
	}{
		{"idle", true, true, true},
		{"done", true, true, true},
		{"blocked", false, false, true},
		{"working", false, false, false},
		{"", false, false, false},
	}
	for _, tc := range cases {
		t.Run("status="+tc.status, func(t *testing.T) {
			if got := autoSendParked(tc.status); got != tc.autoSend {
				t.Errorf("autoSendParked(%q) = %v, want %v", tc.status, got, tc.autoSend)
			}
			if got := sessionRenameParked(tc.status); got != tc.sessionRename {
				t.Errorf("sessionRenameParked(%q) = %v, want %v", tc.status, got, tc.sessionRename)
			}
			if got := autoAcceptParked(tc.status); got != tc.autoAcpt {
				t.Errorf("autoAcceptParked(%q) = %v, want %v", tc.status, got, tc.autoAcpt)
			}
		})
	}
}

// sessionRenameParked is the one that folds case and spacing, because it reads
// a status that has been round-tripped through the roster rather than taken
// straight off a transition.
func TestSessionRenameParkedFoldsCaseAndSpace(t *testing.T) {
	for _, s := range []string{"Done", " done ", "DONE", "Idle"} {
		if !sessionRenameParked(s) {
			t.Errorf("sessionRenameParked(%q) = false, want true", s)
		}
	}
}

// domain.AgentBusy is the inverse gate — every hand-out path asks it before
// typing — so "done" must read as NOT busy for the same reason.
func TestAgentBusyTreatsDoneAsParked(t *testing.T) {
	cases := []struct {
		status string
		busy   bool
	}{
		{"idle", false},
		{"done", false},
		{"working", true},
		{"blocked", true},
		// An unreported status is not evidence of work; the roster records it
		// for a pane herdr has not described yet.
		{"", false},
		// Guarded deliberately: "detected" is a real roster status and IS
		// busy, which is what stops a hand-out landing in a starting agent.
		{"detected", true},
	}
	for _, tc := range cases {
		if got := domain.AgentBusy(tc.status); got != tc.busy {
			t.Errorf("AgentBusy(%q) = %v, want %v", tc.status, got, tc.busy)
		}
	}
}
