package domain

import "time"

// NodeInfo is one installation sharing the store: what its daemon reported
// about itself the last time it checked in.
type NodeInfo struct {
	ID string
	// Label is what the operator sees beside a remote agent ("name@label").
	// The machine's hostname unless configured.
	Label      string
	HapVersion string
	StartedAt  time.Time
	// LastSeen is the daemon's last heartbeat. Absence of a recent one is
	// "that machine's daemon is not reporting", never "its agents are gone".
	LastSeen time.Time
	// WatchingUntil is set by a TUI on that node while it is open, so other
	// nodes' daemons publish their roster faster for it.
	WatchingUntil time.Time
}

// NodeHeartbeat is how often a daemon refreshes its nodes row. It MUST equal
// daemonhealth.HeartbeatInterval (the domain cannot import it; a test in
// internal/daemon pins the two together).
const NodeHeartbeat = 10 * time.Second

// nodeStaleAfter is how long after its last heartbeat a node reads as stale:
// three missed beats, comfortably past one sync interval on a shared store.
const nodeStaleAfter = 3 * NodeHeartbeat

// NodeStale reports whether a node's daemon has stopped reporting. A node that
// never reported (zero LastSeen) is stale.
func NodeStale(n NodeInfo, now time.Time) bool {
	return n.LastSeen.IsZero() || now.Sub(n.LastSeen) > nodeStaleAfter
}

// NodeWatching reports whether a TUI on that node is watching the fleet.
func NodeWatching(n NodeInfo, now time.Time) bool {
	return !n.WatchingUntil.IsZero() && now.Before(n.WatchingUntil)
}

// NodeLabelOrID is the display label, falling back to the id's first eight
// characters — the same shape a git short hash has, for the same reason.
func NodeLabelOrID(n NodeInfo) string {
	if n.Label != "" {
		return n.Label
	}
	if len(n.ID) > 8 {
		return n.ID[:8]
	}
	return n.ID
}
