package logging

import (
	"log/slog"
	"sync"
)

// warnedOnce latches which keys have already been warned about in this process.
var warnedOnce sync.Map

// WarnOnce emits a warning at most once per process, keyed by key.
//
// For conditions that are re-evaluated on a timer and stay true: the operator
// needs to be told, but repeating it every tick is pure volume. The TUI's
// refresh runs every 2s, so a persistent failure there is 43,200 identical lines
// a day — into the same file the daemon writes to. The same shape once made a
// live plugin log carry 1,842 copies of three config warnings.
//
// Keyed explicitly rather than by the formatted message so the caller decides
// what "the same condition" means: an error string that embeds a changing path
// or errno would otherwise defeat the latch and restore the flood.
func WarnOnce(key, msg string, args ...any) {
	if _, seen := warnedOnce.LoadOrStore(key, struct{}{}); seen {
		return
	}
	slog.Warn(msg, args...)
}

// ResetWarnOnceForTest clears the latch so a test can observe a warning an
// earlier test in the same process already consumed.
func ResetWarnOnceForTest() { warnedOnce.Clear() }
