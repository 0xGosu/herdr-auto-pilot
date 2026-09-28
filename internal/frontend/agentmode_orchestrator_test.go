package frontend_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// TestOrchestratorPromotesAManualAgent is the positive control for every
// orchestrator refusal below: without it they all pass on code that refuses
// the orchestrator unconditionally, and the feature ships dead.
func TestOrchestratorPromotesAManualAgent(t *testing.T) {
	app, fake := claudeApp(t, domain.AgentModeManual)
	app.Author = domain.OrchestratorAuthor
	change, err := app.SetAgentMode(context.Background(), "w1:p1", "auto", fastModes)
	if err != nil || change.Mode != domain.AgentModeAuto {
		t.Fatalf("orchestrator promotion: mode %q, err %v; want auto", change.Mode, err)
	}
	if chords, _ := fake.counts(); chords == 0 {
		t.Fatal("no chord was sent")
	}
}

// killErrorStore fails the pause read, which must read as PAUSED.
type killErrorStore struct{ ports.FrontendStore }

func (killErrorStore) LatestKillEvent(context.Context) (*domain.KillEvent, error) {
	return nil, errors.New("induced kill-state failure")
}

func TestOrchestratorModeChangeFailsClosedOnAnUnreadablePause(t *testing.T) {
	app, fake := claudeApp(t, domain.AgentModeManual)
	app.Store = killErrorStore{FrontendStore: app.Store}
	app.Author = domain.OrchestratorAuthor
	_, err := app.SetAgentMode(context.Background(), "w1:p1", "auto", fastModes)
	if chords, _ := fake.counts(); !errors.Is(err, frontend.ErrModePaused) || chords != 0 {
		t.Fatalf("err = %v, chords = %d; want ErrModePaused and none", err, chords)
	}
}

// TestOrchestratorMayNotChangeADisabledAgentsMode: hap disable stops the
// orchestrator's keystrokes here as it does everywhere else; the operator is
// the control.
func TestOrchestratorMayNotChangeADisabledAgentsMode(t *testing.T) {
	for _, author := range []string{domain.OrchestratorAuthor, domain.OperatorAuthor} {
		t.Run(author, func(t *testing.T) {
			app, fake := claudeApp(t, domain.AgentModeManual)
			if err := app.SetAgentDisabled(context.Background(), "w1:p1", true); err != nil {
				t.Fatal(err)
			}
			app.Author = author
			_, err := app.SetAgentMode(context.Background(), "w1:p1", "auto", fastModes)
			chords, _ := fake.counts()
			if author == domain.OrchestratorAuthor {
				if !errors.Is(err, frontend.ErrModeAgentDisabled) || chords != 0 {
					t.Fatalf("err = %v, chords = %d; want ErrModeAgentDisabled and none", err, chords)
				}
				return
			}
			if err != nil || chords == 0 {
				t.Fatalf("operator refused on a disabled agent: err = %v, chords = %d", err, chords)
			}
		})
	}
}

// TestSetAgentModeRecordsNothingAfterAnUnverifiedPress: a press no settled read
// confirmed leaves change.Mode at the reading from BEFORE it, which must not be
// recorded as the mode the agent is in.
func TestSetAgentModeRecordsNothingAfterAnUnverifiedPress(t *testing.T) {
	app, fake := claudeApp(t, domain.AgentModeManual)
	log := withStream(t, app)
	app.Author = domain.OperatorAuthor
	fake.render = func(m domain.AgentMode) string {
		if m == domain.AgentModeManual {
			return renderClaude(m)
		}
		return "● working\n" // the footer vanishes after the press
	}
	if _, err := app.SetAgentMode(context.Background(), "w1:p1", "auto", fastModes); !errors.Is(err, frontend.ErrModeUnreadable) {
		t.Fatalf("err = %v; want ErrModeUnreadable", err)
	}
	if evs, err := log.Since(context.Background(), 0, 10); err != nil || len(evs) != 0 {
		t.Fatalf("an unverified mode was recorded: %+v, %v", evs, err)
	}
}

// TestOrchestratorMayNotLeavePlanMode is the plan exception, enforced against
// the LIVE read: the event the orchestrator acts on is older than this call,
// and the operator may have switched the agent to plan since. The operator
// case is the control — without it the refusal would pass on code that refuses
// every plan-mode agent.
func TestOrchestratorMayNotLeavePlanMode(t *testing.T) {
	for _, author := range []string{domain.OrchestratorAuthor, domain.OperatorAuthor} {
		t.Run(author, func(t *testing.T) {
			app, fake := claudeApp(t, domain.AgentModePlan)
			app.Author = author
			_, err := app.SetAgentMode(context.Background(), "w1:p1", "auto", fastModes)
			chords, _ := fake.counts()
			if author == domain.OrchestratorAuthor {
				if !errors.Is(err, frontend.ErrModePlanHeld) || chords != 0 {
					t.Fatalf("orchestrator left plan: err = %v, chords = %d; want ErrModePlanHeld and none", err, chords)
				}
				return
			}
			if err != nil || chords == 0 {
				t.Fatalf("operator refused out of plan: err = %v, chords = %d", err, chords)
			}
		})
	}
}

// TestOrchestratorMayNotChangeAModeWhilePaused: a paused herd refuses the
// orchestrator's keystrokes, and the operator's are untouched (the control).
func TestOrchestratorMayNotChangeAModeWhilePaused(t *testing.T) {
	for _, author := range []string{domain.OrchestratorAuthor, domain.OperatorAuthor} {
		t.Run(author, func(t *testing.T) {
			app, fake := claudeApp(t, domain.AgentModeManual)
			if _, err := app.Pause(context.Background()); err != nil {
				t.Fatal(err)
			}
			app.Author = author
			_, err := app.SetAgentMode(context.Background(), "w1:p1", "auto", fastModes)
			chords, _ := fake.counts()
			if author == domain.OrchestratorAuthor {
				if !errors.Is(err, frontend.ErrModePaused) || chords != 0 {
					t.Fatalf("paused orchestrator: err = %v, chords = %d; want ErrModePaused and none", err, chords)
				}
				return
			}
			if err != nil || chords == 0 {
				t.Fatalf("paused operator refused: err = %v, chords = %d", err, chords)
			}
		})
	}
}

// TestSetAgentModeRecordsTheModeUnderItsAuthor: a set puts the mode the agent
// ended in on the stream, authored by the caller and with no promote= — and
// that mark makes the daemon's later reading of the same mode a duplicate, so
// an operator's chosen manual is never offered for promotion.
func TestSetAgentModeRecordsTheModeUnderItsAuthor(t *testing.T) {
	app, _ := claudeApp(t, domain.AgentModeAcceptEdits)
	log := withStream(t, app)
	app.Author = domain.OperatorAuthor
	if _, err := app.SetAgentMode(context.Background(), "w1:p1", "manual", fastModes); err != nil {
		t.Fatal(err)
	}
	evs, err := log.Since(context.Background(), 0, 10)
	if err != nil || len(evs) != 1 {
		t.Fatalf("stream = %+v, %v; want one event", evs, err)
	}
	if got := evs[0].Line(); !strings.Contains(got, "agent.mode agent=w1:p1 mode=manual by=operator") {
		t.Fatalf("event = %q", got)
	}
	observed := domain.AgentModeStreamEvent("w1:p1", "w1:p1", "claude", domain.AgentModeManual, true)
	observed.Author = "daemon"
	if seq, err := log.Append(context.Background(), observed); err != nil || seq != 0 {
		t.Fatalf("the daemon's reading of the operator's chosen mode was appended (seq %d, %v) — it would offer promote=", seq, err)
	}
}
