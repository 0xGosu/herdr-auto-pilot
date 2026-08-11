package cli

import "io"

// SetDeprecationOutput redirects the "this verb moved" note, which normally
// goes to stderr so it cannot corrupt the tab-separated listings these verbs
// print. It returns a function restoring the previous writer.
func SetDeprecationOutput(w io.Writer) func() {
	prev := deprecationOut
	deprecationOut = w
	return func() { deprecationOut = prev }
}
