package frontend

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/daemonhealth"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// An action that only the daemon can perform must be refused when no daemon can
// perform it, BEFORE anything is written.
//
// Queuing it anyway would look like success and then do nothing — indefinitely,
// with the row sitting in a healthy-looking queue. That is worse than the old
// behaviour it replaces, where the front end did the work itself and any
// failure was immediate and visible.
func TestAnActionIsRefusedWithNoDaemonRunning(t *testing.T) {
	app := appWithDaemon(t, false, 0, "")
	err := app.requireLiveDaemon()
	if err == nil {
		t.Fatal("queued an agent action with no daemon to run it")
	}
	if !errors.Is(err, ErrDaemonUnavailable) {
		t.Errorf("error %v does not wrap ErrDaemonUnavailable; callers cannot tell "+
			"'we never tried' from 'the agent refused'", err)
	}
	if !strings.Contains(err.Error(), "hap daemon --ensure") {
		t.Errorf("error = %q; want the remedy the front ends already render", err)
	}
}

// A HUNG daemon holds the lock, so it reads as running — but it is by
// definition not draining anything, which is exactly the case a bare
// "is it running" check would wave through.
func TestAnActionIsRefusedByAHungDaemon(t *testing.T) {
	app := appWithDaemon(t, true, 4242, "")
	if err := daemonhealth.Write(app.StateDir, daemonhealth.Health{
		PID:         4242,
		HeartbeatAt: time.Now().Add(-daemonhealth.StaleAfter - time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	err := app.requireLiveDaemon()
	if err == nil {
		t.Fatal("queued an agent action against a hung daemon")
	}
	if !errors.Is(err, ErrDaemonUnavailable) {
		t.Errorf("error %v does not wrap ErrDaemonUnavailable", err)
	}
	if !strings.Contains(err.Error(), "progress") {
		t.Errorf("error = %q; want it to say the daemon is stuck rather than absent", err)
	}
}

// A daemon whose binary was replaced underneath it keeps running, but every
// child it spawns from that path fails. It cannot be trusted with an action.
func TestAnActionIsRefusedByADaemonWhoseBinaryWentAway(t *testing.T) {
	app := appWithDaemon(t, true, 4242, "")
	if err := daemonhealth.Write(app.StateDir, daemonhealth.Health{
		PID: 4242, HeartbeatAt: time.Now(), BinaryReplaced: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := app.requireLiveDaemon(); err == nil {
		t.Fatal("queued an agent action against a daemon with no binary")
	}
}

func TestAHealthyDaemonAcceptsActions(t *testing.T) {
	app := appWithDaemon(t, true, 4242, "")
	if err := daemonhealth.Write(app.StateDir, daemonhealth.Health{
		PID: 4242, HeartbeatAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := app.requireLiveDaemon(); err != nil {
		t.Fatalf("refused a healthy daemon: %v", err)
	}
}

// A timeout must never be reported as a failed action. The daemon still holds
// the claim and may deliver a moment later, so saying "it did not happen" is a
// claim the queue can contradict.
func TestAwaitTimeoutIsNotReportedAsAFailure(t *testing.T) {
	app := &App{Store: &stuckActionStore{}}
	_, err := app.AwaitAgentAction(context.Background(), 1, 200*time.Millisecond)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if !strings.Contains(err.Error(), "still queued") {
		t.Errorf("error = %q; want it to say the request is still in flight", err)
	}
}

func TestAwaitReportsTheDaemonsFailureVerbatim(t *testing.T) {
	app := &App{Store: &finishedActionStore{
		act: domain.AgentAction{
			ID: 1, Status: domain.AgentActionFailed, Error: "the pane is no longer readable",
		},
	}}
	_, err := app.AwaitAgentAction(context.Background(), 1, time.Second)
	if err == nil || err.Error() != "the pane is no longer readable" {
		t.Errorf("err = %v; want the daemon's own sentence, unwrapped, so the caller can add its own context", err)
	}
}

func TestAwaitReturnsTheResultOnSuccess(t *testing.T) {
	app := &App{Store: &finishedActionStore{
		act: domain.AgentAction{ID: 1, Status: domain.AgentActionDone, Result: `{"mode":"plan"}`},
	}}
	res, err := app.AwaitAgentAction(context.Background(), 1, time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res != `{"mode":"plan"}` {
		t.Errorf("result = %q, want the executor's report", res)
	}
}

// The two fakes below embed the interface rather than a real store: these
// tests exercise the WAIT, not the queue, so every other method is deliberately
// nil and would panic if the wait ever reached for one.
type stuckActionStore struct{ ports.FrontendStore }

func (s *stuckActionStore) AgentActionByID(context.Context, int64) (*domain.AgentAction, error) {
	return &domain.AgentAction{ID: 1, Status: domain.AgentActionRunning}, nil
}

type finishedActionStore struct {
	ports.FrontendStore
	act domain.AgentAction
}

func (s *finishedActionStore) AgentActionByID(context.Context, int64) (*domain.AgentAction, error) {
	a := s.act
	return &a, nil
}
