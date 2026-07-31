package config

import (
	"log/slog"
	"sync"
)

// warnedConfigIssues latches which deprecation warnings this process has
// already emitted, keyed by message.
var warnedConfigIssues sync.Map

// warnOnce emits a config deprecation warning at most once per process.
//
// Load runs on every daemon reload nudge, every TUI refresh and every CLI
// invocation, and each deprecated key in the file warns on each of those. The
// warning is worth making once — it tells the operator to edit their config —
// but repeating it forever is pure volume: a live 0.5.19 log carried 1842
// copies of three such lines in a single tail window, and they were the bulk of
// a 1.9 GB file. Latching per message (not per call site) also collapses the
// repeats across the several Load calls one command makes.
//
// Keyed on the message alone rather than message+path: the point is to tell
// the operator once about a key they must edit, and hap reads one config file.
func warnOnce(msg string, args ...any) {
	if _, seen := warnedConfigIssues.LoadOrStore(msg, struct{}{}); seen {
		return
	}
	slog.Warn(msg, args...)
}

// resetWarnOnceForTest clears the latch so a test can observe a warning that an
// earlier test in the same process already consumed.
func resetWarnOnceForTest() {
	warnedConfigIssues.Clear()
}
