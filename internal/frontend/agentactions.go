package frontend

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/control"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// ErrDaemonUnavailable reports that an action needing a live daemon was refused
// before anything was queued.
//
// It is a distinct error rather than a sentence so callers can tell "we did not
// even try" from "we tried and the agent refused" — the two need different
// operator responses, and only the first is fixed by starting a daemon.
var ErrDaemonUnavailable = errors.New("no healthy hap daemon is running")

// actionPollInterval is how often a waiting front end re-reads a queued action.
//
// It is short because the daemon acts on the nudge, not on a timer: the normal
// case is one or two polls. The cost of a miss is an operator watching a
// spinner, so this is tuned for the common case rather than for the queue.
const actionPollInterval = 100 * time.Millisecond

// DefaultActionTimeout bounds how long a front end waits for a queued action.
//
// Generous relative to the sub-second normal case, because the alternative is
// worse than waiting: a timeout does NOT cancel the action — the daemon still
// holds the claim and still delivers — so reporting one early tells the
// operator nothing happened while it is in fact happening.
const DefaultActionTimeout = 30 * time.Second

// requireLiveDaemon refuses an action that only the daemon can perform when no
// daemon can perform it.
//
// The front ends no longer touch herdr, so queuing such an action with nothing
// draining the queue would leave the operator's confirm sitting in the database
// indefinitely — silently, since the row looks healthy. Config writes,
// record-only confirms and dismissals deliberately do NOT come through here:
// they need no herdr and must keep working with the daemon down, which is the
// ordinary first-run order.
//
// A HUNG daemon is refused too. It holds the lock, so it looks alive, but it is
// by definition not draining anything.
func (a *App) requireLiveDaemon() error {
	if a.InDaemon {
		// This IS the daemon. It has no lock file of its own to read, and it
		// must never wait on a queue only it can drain — see App.InDaemon.
		return nil
	}
	h := a.AssessDaemonHealth()
	switch {
	case !h.Running:
		return fmt.Errorf("%w; start one with `hap daemon --ensure`", ErrDaemonUnavailable)
	case h.Hung:
		return fmt.Errorf("%w: the daemon is running but has not made progress for %s; restart it with `hap daemon --ensure`",
			ErrDaemonUnavailable, h.HeartbeatAge.Round(time.Second))
	case h.BinaryReplaced:
		return fmt.Errorf("%w: the daemon's binary was replaced underneath it; hand it over with `hap daemon --ensure`",
			ErrDaemonUnavailable)
	}
	return nil
}

// queueAgentAction refuses early if no daemon can run the action, writes the
// row, and nudges.
//
// The nudge is KindWake rather than KindReload on purpose: waking has no case
// in the daemon's switch and falls straight to the drain tail, so an operator
// action no longer forces a full config reload with its classifier, LLM adapter
// and embedder rebuild. A lost nudge costs only latency — the periodic sweep
// drains the same queue.
func (a *App) queueAgentAction(ctx context.Context, act domain.AgentAction) (int64, error) {
	if err := a.requireLiveDaemon(); err != nil {
		return 0, err
	}
	if act.Author == "" {
		act.Author = a.Author
	}
	if act.CreatedAt.IsZero() {
		act.CreatedAt = time.Now()
	}
	id, err := a.Store.EnqueueAgentAction(ctx, act)
	if err != nil {
		return 0, err
	}
	a.nudge(ctx, control.KindWake)
	return id, nil
}

// AwaitAgentAction blocks until a queued action reaches a terminal status and
// reports its error text (empty on success) plus its JSON result.
//
// A timeout is reported as such and never as a failure of the action: the
// daemon still holds the claim and may still deliver, so telling the operator
// it did not happen would be a lie the queue can contradict a second later.
func (a *App) AwaitAgentAction(ctx context.Context, id int64, timeout time.Duration) (result string, err error) {
	if timeout <= 0 {
		timeout = DefaultActionTimeout
	}
	deadline := time.Now().Add(timeout)
	for {
		act, readErr := a.Store.AgentActionByID(ctx, id)
		if readErr != nil {
			return "", readErr
		}
		if act == nil {
			return "", fmt.Errorf("queued action %d disappeared", id)
		}
		if act.Status == domain.AgentActionFailed {
			return act.Result, errors.New(act.Error)
		}
		if act.Status == domain.AgentActionDone {
			return act.Result, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("the daemon has not finished the request after %s; it is still queued — check `hap audit`",
				timeout.Round(time.Second))
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(actionPollInterval):
		}
	}
}
