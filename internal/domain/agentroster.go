package domain

import "time"

// RosterAgent is one agent as the daemon last published it.
//
// It is deliberately NOT an AgentTransition. A transition is an EVENT — a
// moment at which an agent's status changed, carrying fields the daemon sets
// for its own pipeline (RetryAuditID, ManualCapture, AutoIdleSend) that mean
// nothing to a reader. A roster row is the current STATE, plus the two things
// only a snapshot can carry: when it was observed, and the cwd that costs a
// subprocess to read.
type RosterAgent struct {
	// NodeID is the installation whose daemon published this row.
	NodeID      string
	AgentID     string
	PaneID      string
	TabID       string
	WorkspaceID string
	AgentType   string
	Status      string
	// TerminalID is herdr's per-terminal identity. Herdr recycles pane ids and
	// an agent id IS a pane id, so a changed TerminalID means a DIFFERENT
	// agent on the same id, never the same one moving.
	TerminalID string
	// Cwd is the agent's working directory, "" when it has not been read.
	// Absence is "not read", never "the agent has no cwd".
	Cwd       string
	CwdReadAt time.Time
	SeenAt    time.Time
}

// Transition renders the roster row as the AgentTransition shape the decision
// core and the existing view code already speak.
//
// The pipeline-only fields stay zero by construction: a roster row is state,
// and nothing about it can imply a manual capture or an idle hand-out.
func (r RosterAgent) Transition() AgentTransition {
	return AgentTransition{
		AgentID:     r.AgentID,
		AgentType:   r.AgentType,
		PaneID:      r.PaneID,
		TabID:       r.TabID,
		WorkspaceID: r.WorkspaceID,
		Status:      r.Status,
		TerminalID:  r.TerminalID,
		At:          r.SeenAt,
	}
}

// RosterAgentFrom builds a roster row from a live transition.
func RosterAgentFrom(tr AgentTransition, seenAt time.Time) RosterAgent {
	pane := tr.PaneID
	if pane == "" {
		// herdr identifies most agents by their pane id and the transition
		// carries both; falling back keeps an empty PaneID from dropping the
		// row's only usable address.
		pane = tr.AgentID
	}
	return RosterAgent{
		AgentID: tr.AgentID, PaneID: pane, TabID: tr.TabID,
		WorkspaceID: tr.WorkspaceID, AgentType: tr.AgentType,
		Status: tr.Status, TerminalID: tr.TerminalID, SeenAt: seenAt,
	}
}

// RosterStaleAfter bounds how old a published roster may be before a reader
// treats it as unknown rather than current.
//
// It has to clear the daemon's periodic sweep with room to spare, because on a
// quiescent herd with no TUI open that sweep is the only publisher — the
// event-driven path republishes an agent the moment anything about it changes,
// so a roster this old is one nothing has happened to. Too tight and an idle
// herd reads as "no daemon"; too loose and a daemon that died minutes ago still
// looks authoritative.
const RosterStaleAfter = 3 * time.Minute

// RosterFresh reports whether a roster published at publishedAt can still be
// trusted as of now. A zero publishedAt means no daemon has ever published,
// which is UNKNOWN — never "no agents are running".
func RosterFresh(publishedAt, now time.Time) bool {
	if publishedAt.IsZero() {
		return false
	}
	return now.Sub(publishedAt) <= RosterStaleAfter
}

// RosterCwd is one agent's working directory together with the terminal it was
// read under.
//
// An empty Cwd is a recorded ATTEMPT, not a missing value: it still stamps
// cwd_read_at, so a pane herdr cannot describe rides the same TTL as one it
// can rather than being re-asked on every pass forever.
//
// The terminal travels with it because the read happens off the daemon's
// select loop: that loop can publish a recycled pane on the same agent id
// while the lookup is still running, and a directory belonging to a process
// that has already exited must not land on its successor's row.
type RosterCwd struct {
	Cwd        string
	TerminalID string
}

// NodeAgent identifies an agent fleet-wide: herdr pane ids repeat on every
// machine, so an agent id alone is ambiguous once several nodes share a store.
type NodeAgent struct {
	NodeID  string
	AgentID string
}
