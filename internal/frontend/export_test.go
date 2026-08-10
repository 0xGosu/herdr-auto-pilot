package frontend

// CwdTTLForTest exposes the working-directory cache TTL to the external test
// package so expiry can be exercised at the real boundary instead of a
// hard-coded duplicate that would silently drift from it.
const CwdTTLForTest = cwdTTL

// TUILimitSweepIntervalForTest exposes the instance-limit throttle for the same
// reason: the test asserts at the real boundary, not a copy of it.
const TUILimitSweepIntervalForTest = tuiLimitSweepInterval

// TaskSourceLimitForTest exposes the max_tasks lookup so a test can assert a
// configured source is RECOGNIZED. The failure it guards is silent — an
// unrecognized source reads as uncapped rather than erroring.
func (a *App) TaskSourceLimitForTest(agent, locator string) int {
	return a.taskSourceLimit(agent, locator)
}
