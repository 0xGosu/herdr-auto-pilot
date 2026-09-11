package cli

import "time"

// SetStreamIdlePolling overrides the idle back-off: the slower interval and how
// long a stream must go without an event before using it. It returns a
// function restoring both.
func SetStreamIdlePolling(interval, after time.Duration) func() {
	prevInterval, prevAfter := streamIdlePollInterval, streamIdleAfter
	streamIdlePollInterval, streamIdleAfter = interval, after
	return func() { streamIdlePollInterval, streamIdleAfter = prevInterval, prevAfter }
}

// SetStreamGapRecheck overrides how long a caught-up stream goes without
// re-asking for the retained floor. It returns a function restoring it.
func SetStreamGapRecheck(d time.Duration) func() {
	prev := streamGapRecheck
	streamGapRecheck = d
	return func() { streamGapRecheck = prev }
}
