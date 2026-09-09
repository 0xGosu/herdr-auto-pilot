//go:build !unix

package fdprobe

// read is the no-evidence answer on a platform with no descriptor budget to
// read. Every field stays zero, and OpenKnown stays false, so a reader renders
// "unavailable" rather than a made-up number.
func read() Probe { return Probe{} }
