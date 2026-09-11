package cli

import (
	"io"
	"time"
)

// SetStreamPollInterval shortens how often `hap stream` polls, returning a
// function restoring the previous interval.
func SetStreamPollInterval(d time.Duration) func() {
	prev := streamPollInterval
	streamPollInterval = d
	return func() { streamPollInterval = prev }
}

// SetDeprecationOutput redirects the "this verb moved" note, which normally
// goes to stderr so it cannot corrupt the tab-separated listings these verbs
// print. It returns a function restoring the previous writer.
func SetDeprecationOutput(w io.Writer) func() {
	prev := deprecationOut
	deprecationOut = w
	return func() { deprecationOut = prev }
}
