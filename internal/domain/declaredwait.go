package domain

import (
	"fmt"
	"strings"
	"time"
)

// A wait an agent DECLARED for itself, as opposed to one hap inferred.
//
// #508 item 3 asked for two things and BackgroundWorkRunning only answered one
// of them. That predicate reads an INDICATOR off the pane, so it can only ever
// see work the agent handed to a background shell and then parked beside. Two
// shapes stay invisible to it:
//
//   - a FOREGROUND build. The agent reports `working` for the whole quarter of
//     an hour, and both of BackgroundWorkRunning's callers only look at parked
//     agents, so it never reaches the inference at all.
//   - anything whose indicator hap does not know. The rule there is deliberate
//     (an unrecognized screen answers false), but it means a codex agent, or a
//     claude build that paints nothing, is reclaimed as if it had stalled.
//
// A declaration has the property an indicator cannot: it comes from the agent
// itself, while it is still running, which is direct evidence of liveness at
// that instant. It is also BOUNDED, which is what makes it safe to honour where
// an indicator is not — an indicator can outlive the work (a dev server, a
// `tail -f`), so it may only ever defer, while a wait expires on its own.
//
// It is deliberately NOT a snooze:
//
//   - a snooze is the OPERATOR saying "this agent is finished, stop asking";
//     it has no expiry and is lifted by the transition to working.
//   - a wait is the AGENT saying "I am busy until roughly T"; it expires on the
//     clock and is NOT lifted by the transition to working — the parked →
//     working → parked flap a build's own output causes is exactly what
//     defeated the episode latch in #526, and a state cleared by that flap
//     could not survive the thing it exists to cover.
//
// What it gates is the same narrow set the snooze gates — the notices and
// hand-outs about an agent's QUEUE (daemon.queueNoticeWithheld,
// daemon.eligibleIdleAgents, daemon.reclaimStrandedTasks). It must never reach
// WithAgentAutomation, deliverreply.go, autoaccept.go or generatedtask.go's
// barrier: an agent that is busy building still wants its approvals answered,
// and taking that away is `hap disable`, which is a different switch.
type AgentWait struct {
	// Until is when the declaration lapses, on the OWNING node's clock. Zero
	// means no wait was ever declared, which is also what an expired one
	// eventually looks like to a reader — see Active.
	Until time.Time
	// Reason is the agent's own words, for the operator's listing. Advisory:
	// nothing branches on it.
	Reason string
}

// Active reports whether the declaration still stands at now.
//
// Expiry is READ, never written: a sweep that cleared lapsed rows would be an
// unconditional periodic write, which arms the turso push debounce on every
// tick — the leak the "a periodic write is CONDITIONAL" rule names. A lapsed
// row simply reads as no wait and is overwritten by the next declaration.
func (w AgentWait) Active(now time.Time) bool {
	return !w.Until.IsZero() && now.Before(w.Until)
}

// Remaining is how much of the declaration is left at now, floored at zero.
func (w AgentWait) Remaining(now time.Time) time.Duration {
	if !w.Active(now) {
		return 0
	}
	return w.Until.Sub(now)
}

const (
	// MinDeclaredWait is the shortest declaration worth taking. Below it the
	// agent is better off saying nothing: the sweep runs once a minute, so a
	// wait shorter than one sweep cannot change any verdict, and accepting it
	// would let a typo ("hap wait 30" read as nanoseconds) read as a real
	// declaration that silently does nothing.
	MinDeclaredWait = time.Minute
	// MaxDeclaredWait is the longest one, and it is a safety bound rather than
	// a convenience. An unbounded wait is `hap disable` by another name, minus
	// every place that says so to the operator; a bounded one always ends with
	// hap looking at the agent again. Two hours comfortably covers the shapes
	// #508 reported — a cold native build, a CI run — while staying inside how
	// long an operator will tolerate a checklist item sitting at "[-]".
	//
	// A longer wait is asked for by declaring again, which is not a loophole:
	// a renewal requires a LIVE process to run the verb a second time, and
	// that liveness is the whole evidence a declaration carries. What is
	// bounded absolutely is the one place a renewal could still pin state —
	// see daemon.reclaimStrandedTasks' give-up branch.
	MaxDeclaredWait = 2 * time.Hour
	// maxWaitReasonRunes bounds what is stored. The reason is free text from an
	// agent and is rendered in `hap agents`, so it is clipped rather than
	// refused: a long reason is still a real declaration.
	maxWaitReasonRunes = 200
)

// NormalizeWaitReason folds an agent's free-text reason to one storable line.
func NormalizeWaitReason(reason string) string {
	r := strings.Join(strings.Fields(strings.ReplaceAll(reason, "\n", " ")), " ")
	if runes := []rune(r); len(runes) > maxWaitReasonRunes {
		return string(runes[:maxWaitReasonRunes])
	}
	return r
}

// ValidateWaitDuration bounds a declared wait, naming the bound it broke.
//
// A zero duration is NOT an error here: it is how every surface spells "clear
// the wait", and refusing it would leave an agent that finished early with no
// way to say so.
func ValidateWaitDuration(d time.Duration) error {
	switch {
	case d == 0:
		return nil
	case d < 0:
		return fmt.Errorf("a wait cannot be negative")
	case d < MinDeclaredWait:
		return fmt.Errorf("a wait of %s is shorter than the %s sweep and would change nothing; declare at least %s",
			d, MinDeclaredWait, MinDeclaredWait)
	case d > MaxDeclaredWait:
		return fmt.Errorf("a wait of %s is longer than the %s ceiling; declare a shorter one and renew it, or use `hap disable` if hap should keep its hands off the agent entirely",
			d, MaxDeclaredWait)
	}
	return nil
}

// ShortDuration renders a remaining wait for a one-line listing: "14m", "1h6m",
// "45s". time.Duration's own String is unusable there — it carries sub-second
// precision an operator never wants ("13m59.612s"), and the rounding has to
// happen where the unit is chosen or "1h0m0s" comes back for a round hour.
func ShortDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Round(time.Second)/time.Second))
	}
	d = d.Round(time.Minute)
	if h := int(d / time.Hour); h > 0 {
		return fmt.Sprintf("%dh%dm", h, int((d%time.Hour)/time.Minute))
	}
	return fmt.Sprintf("%dm", int(d/time.Minute))
}
