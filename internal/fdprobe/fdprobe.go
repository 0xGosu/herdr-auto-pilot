// Package fdprobe answers, at the moment something failed, the one question a
// wedged process cannot answer from a log line afterwards: is it out of file
// descriptors?
//
// It exists for a specific failure. Under the turso engine on macOS the sync
// engine's TLS handshake started failing with
//
//	tls: failed to verify certificate: SecPolicyCreateSSL error: 0
//
// which is crypto/x509's darwin verifier reporting that SecPolicyCreateSSL
// returned NULL — a Security-framework allocation failure, not anything about
// the certificate. Descriptor exhaustion is the leading explanation (the
// policy machinery reaches trustd over XPC and reads the framework's own
// resource bundle, and macOS still ships a 256 soft limit to many shells), but
// it was never MEASURED: by the time an operator ran `hap status` the daemon
// had been restarted. So the probe records the evidence with the failure
// rather than leaving the next occurrence to another round of guessing.
//
// Exhausted is the load-bearing field, and it is the only one that is
// trustworthy everywhere: Open counts /proc/self/fd, which does not exist on
// macOS — precisely the platform this was written for — so a zero Open is
// "unavailable", never "none open". Exhausted is a real syscall on both.
package fdprobe

// Probe is one reading of the process's descriptor budget.
type Probe struct {
	// Soft and Hard are RLIMIT_NOFILE; 0 means the limit could not be read.
	Soft, Hard uint64
	// Open is the number of open descriptors, or 0 where the platform offers
	// no cheap way to count them (macOS). Never read it as a count of zero.
	Open int
	// OpenKnown distinguishes "zero open" from "cannot count", so a reader
	// never renders an unavailable count as 0.
	OpenKnown bool
	// Exhausted is proof: opening one more descriptor failed with EMFILE or
	// ENFILE. This is the fact worth acting on, and it is available on every
	// platform the daemon runs on.
	Exhausted bool
}

// Read takes a reading. It never errors and never blocks: an unreadable limit
// or an uncountable directory degrades to the zero value for that field.
func Read() Probe { return read() }
