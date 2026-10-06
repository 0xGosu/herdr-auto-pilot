package domain

import (
	"errors"
	"time"
)

// DefaultNodePruneAfter is how long a node must have gone unheard before
// `hap nodes prune` removes its agents when the operator names no age. A week:
// far past any lid-closed weekend, and still well short of the months a parked
// machine's frozen rows would otherwise sit in every fleet read.
const DefaultNodePruneAfter = 7 * 24 * time.Hour

// ErrNodePruneSelf refuses pruning the machine doing the pruning: its own
// daemon is, by construction, the one that is not offline.
var ErrNodePruneSelf = errors.New("this is the node you are on; only another, offline node can be pruned")

// ErrNodeHeardSince refuses a prune whose node reported AFTER the cutoff — it
// came back (or was never gone) between the listing and the delete.
var ErrNodeHeardSince = errors.New("the node has reported since the cutoff; it is not offline long enough to prune")

// OfflineNode is one other node as a prune candidate: who it is, when anything
// was last heard from it (NodeLastHeard; zero = never), and how many agents its
// last roster still lists.
type OfflineNode struct {
	Node      NodeInfo
	LastHeard time.Time
	Agents    int
}

// NodePruneCounts reports what pruning one offline node removed. Only the
// agent-facing rows go: that node's audit trail, decisions, task lists and
// queued actions stay, exactly as PruneAgedRows never sweeps another node's
// rows and never sweeps the audit trail at all.
type NodePruneCounts struct {
	Roster     int64 // agent_roster rows, live and retired
	Tombstones int64 // agent_roster_tombstones
	Locations  int64 // herdr_locations (workspace / tab labels)
	Names      int64 // agent_names rows deleted
	// KeptDisabled counts agent_names rows deliberately KEPT because the
	// operator disabled that agent. A node that comes back reuses its pane
	// ids, and deleting the row would hand that agent back with automation
	// ON — a safety control failing open.
	KeptDisabled int64
	RateRows     int64 // agent_rate
	ErrorRetries int64 // error_retries
	RosterMeta   int64 // roster_meta (the publish stamp)
	Node         int64 // the nodes row itself
	// Escalations counts that node's pending escalations dismissed (the audit
	// rows stay, as dismissed).
	Escalations int64
}

// Rows is the total number of rows deleted (dismissed escalations are an
// update, not a deletion, so they are not counted).
func (c NodePruneCounts) Rows() int64 {
	return c.Roster + c.Tombstones + c.Locations + c.Names + c.RateRows +
		c.ErrorRetries + c.RosterMeta + c.Node
}
