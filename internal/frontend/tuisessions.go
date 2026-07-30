package frontend

import (
	"sync"
	"time"
)

// TUISessionLimiter closes older `hap tui` processes so at most max of them
// stay live, returning the pids it asked to exit. It is an optional capability
// (internal/tuisession implements it): a nil limiter means this front-end is
// not policing instances at all — a CLI run, a TUI that could not register, or
// a platform without the file locks the registry needs.
type TUISessionLimiter interface {
	Enforce(max int) ([]int, error)
}

// tuiLimitSweepInterval throttles the sweep. The TUI calls it from its 2s
// refresh, but the registry only changes when a TUI starts or exits, so a
// slower cadence is plenty — and the case it exists for (a peer that had not
// registered yet when we started, or an operator lowering the limit on a
// running herd) is not urgent.
const tuiLimitSweepInterval = 10 * time.Second

// tuiLimitState is the throttle. It lives on App because both the startup
// sweep and the periodic one go through EnforceTUISessionLimit.
type tuiLimitState struct {
	mu   sync.Mutex
	last time.Time
}

// TUILimitSweep reports one instance-limit sweep: the limit that was in force
// and the TUIs it closed. Max travels with the result so a caller can name the
// setting in its message without re-reading config.
type TUILimitSweep struct {
	Max    int
	Closed []int
}

// EnforceTUISessionLimit closes older TUI processes beyond `[tui]
// max_instances`, and reports what it closed.
//
// It is throttled, so calling it every refresh is cheap; the first call after
// start always runs. A config read error, a registry read error, or no limiter
// at all leaves every TUI running — the limit is a performance guard, never
// something worth failing a front-end over.
func (a *App) EnforceTUISessionLimit() (TUILimitSweep, error) {
	if a.TUISessions == nil {
		return TUILimitSweep{}, nil
	}
	now := time.Now()
	if a.Clock != nil {
		now = a.Clock()
	}
	// Stamped before the work, not after: two refreshes overlapping must not
	// both sweep, and the cost of that choice is only that a failed config
	// read waits out one interval before retrying.
	a.tuiLimit.mu.Lock()
	if !a.tuiLimit.last.IsZero() && now.Sub(a.tuiLimit.last) < tuiLimitSweepInterval {
		a.tuiLimit.mu.Unlock()
		return TUILimitSweep{}, nil
	}
	a.tuiLimit.last = now
	a.tuiLimit.mu.Unlock()

	cfg, err := a.Config()
	if err != nil {
		return TUILimitSweep{}, err
	}
	closed, err := a.TUISessions.Enforce(cfg.TUI.MaxInstances)
	return TUILimitSweep{Max: cfg.TUI.MaxInstances, Closed: closed}, err
}
