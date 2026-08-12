package frontend

import "context"

// CwdTTLForTest exposes the working-directory cache TTL to the external test
// package so expiry can be exercised at the real boundary instead of a
// hard-coded duplicate that would silently drift from it.
const CwdTTLForTest = cwdTTL

// TUILimitSweepIntervalForTest exposes the instance-limit throttle for the same
// reason: the test asserts at the real boundary, not a copy of it.
const TUILimitSweepIntervalForTest = tuiLimitSweepInterval

// NewListHeaderForTest exposes the create-on-demand seed so a test can hold it
// to the gist backend's never-blank rule at the real boundary.
func NewListHeaderForTest(agent string) string { return newListHeader(agent) }

// EnsureListForTest exposes create-on-demand so a test can assert the
// never-blank rule is enforced for EVERY backend, not only the one that breaks.
func (a *App) EnsureListForTest(ctx context.Context, locator, initial string) (bool, error) {
	cfg, err := a.Config()
	if err != nil {
		return false, err
	}
	return a.ensureList(ctx, cfg, locator, initial)
}

// TaskSourceLimitForTest exposes the max_tasks lookup so a test can assert a
// configured source is RECOGNIZED. The failure it guards is silent — an
// unrecognized source reads as uncapped rather than erroring.
func (a *App) TaskSourceLimitForTest(agent, locator string) int {
	return a.taskSourceLimit(agent, locator)
}
