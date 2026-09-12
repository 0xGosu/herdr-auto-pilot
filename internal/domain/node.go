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

// NodeHeartbeat is how often a daemon refreshes its nodes row.
//
// It is deliberately SLOWER than daemonhealth.HeartbeatInterval, which is the
// cadence of the local health FILE. The two used to be one constant, so the
// only way to keep the file responsive was to write a database row every ten
// seconds — with no gate on herd size or on anything having changed, since
// last_seen differs every time. On the shared engine each of those rows is also
// a sync push (the debounce is shorter than the beat, so nothing coalesces), so
// an install with no agents at all still pushed to Turso Cloud around the clock.
//
// The file answers "is this daemon hung", which needs to be current to the
// second. The row answers "is this machine still out there", which no reader
// asks with more precision than nodeStaleAfter. A test in internal/daemon pins
// the daemon's row cadence to this constant.
//
// Why 30s and not a minute, which would cut the writes further: nodeStaleAfter
// is three beats, and it has to stay INSIDE daemon.actionStaleAfter — see
// there. A minute would put staleness at three, past that bound.
const NodeHeartbeat = 30 * time.Second

// nodeStaleAfter is how long after its last heartbeat a node reads as stale:
// three missed beats, comfortably past one sync interval on a shared store.
//
// It is also the gate frontend.requireLiveDaemonFor refuses a remote confirm
// on, and that gives it a CEILING as well as a floor. An operator confirming a
// reply is vouching for what is on an agent's SCREEN, and daemon.actionStaleAfter
// (2 minutes) is how long that vouching stays deliverable. While this window is
// the tighter of the two, a confirm accepted here is one the daemon can still
// honour; past it, the confirm is accepted against a node that was unwatched
// for longer than the screen's own bound. The relationship cannot be expressed
// in the type system (domain cannot import daemon), so
// TestNodeStalenessStaysInsideTheActionBound asserts it — and it is the reason
// NodeHeartbeat is 30s rather than a minute.
const nodeStaleAfter = 3 * NodeHeartbeat

// NodeStale reports whether a node's daemon has stopped reporting. A node that
// never reported (zero LastSeen) is stale.
func NodeStale(n NodeInfo, now time.Time) bool {
	return n.LastSeen.IsZero() || now.Sub(n.LastSeen) > nodeStaleAfter
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
