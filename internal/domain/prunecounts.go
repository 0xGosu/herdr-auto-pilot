package domain

// PruneCounts reports what one row-retention sweep removed or emptied.
//
// It lives here rather than in internal/store because the optional port that
// returns it (ports.RowRetentionPort) is the seam between the daemon and the
// store, and ports may not import an adapter.
type PruneCounts struct {
	AgentActions     int64
	LLMRequests      int64
	LLMDecisions     int64
	Corrections      int64
	LLMRetries       int64
	KillEvents       int64
	TaskReservations int64
	// BlankedPayloads counts finished consult rows whose bulky text was
	// emptied. Kept apart from the deletions because it is a column update:
	// the row survives, so an operator reading the audit trail still finds it.
	BlankedPayloads int64
}

// Rows is the total number of rows deleted.
func (p PruneCounts) Rows() int64 {
	return p.AgentActions + p.LLMRequests + p.LLMDecisions + p.Corrections +
		p.LLMRetries + p.KillEvents + p.TaskReservations
}

// Empty reports whether the sweep changed nothing at all.
func (p PruneCounts) Empty() bool { return p.Rows() == 0 && p.BlankedPayloads == 0 }
