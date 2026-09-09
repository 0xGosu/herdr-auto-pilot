package domain

import "strings"

// SyncFailureProcessLocal reports whether a fleet-sync error names a fault
// that lives in THIS PROCESS rather than out on the network — the only class
// of failure a fresh daemon can clear.
//
// It is the gate on the automatic restart, and its default is the safe one:
// an error it does not recognize answers FALSE. The asymmetry is deliberate.
// Restarting on a fault that is genuinely remote (Turso down, wifi off, a
// revoked token, a laptop asleep) buys nothing and costs the herd its
// in-flight captures and consults, every cooldown, forever — while declining
// to restart on a process-local fault costs one degraded node that says so
// loudly in the TUI and is fixed by `hap daemon --restart`. So an unknown
// error is left to the human.
//
// The shapes below are the ones a restart is known or strongly expected to
// clear:
//
//   - Descriptor exhaustion, whatever raised it.
//   - macOS Security-framework allocation failures. The observed case
//     (2026-09-09) was `SecPolicyCreateSSL error: 0` — crypto/x509's darwin
//     verifier reporting a NULL policy — which recurred every tick until the
//     daemon was restarted and never once afterwards. Note the "error: 0" is
//     not a status code: security.go echoes the NULL return back as the
//     status, so any nonzero value there is a DIFFERENT fault and the match
//     is on the function name alone.
//   - A wedged sync engine reporting its own gate as unavailable.
//
// Everything a remote is entitled to say — timeouts, DNS, refused
// connections, 4xx/5xx — is left out by construction, and the negative list
// is checked FIRST so a message carrying both (a TLS error inside a
// dial timeout, say) is read as the network fault it is.
func SyncFailureProcessLocal(msg string) bool {
	m := strings.ToLower(msg)
	for _, remote := range syncRemoteFaults {
		if strings.Contains(m, remote) {
			return false
		}
	}
	for _, local := range syncProcessLocalFaults {
		if strings.Contains(m, local) {
			return true
		}
	}
	return false
}

// syncRemoteFaults are the shapes that mean the far end, the link or the
// credentials — never this process. Checked first, so they veto.
var syncRemoteFaults = []string{
	"context deadline exceeded",
	"timeout",
	"timed out",
	"no such host",
	"dns",
	"connection refused",
	"connection reset",
	"network is unreachable",
	"host is unreachable",
	"no route to host",
	"unauthorized",
	"forbidden",
	"too many requests",
	"internal server error",
	"bad gateway",
	"service unavailable",
	"gateway timeout",
	"unexpected eof",
}

// syncProcessLocalFaults are the shapes a fresh process is known to clear.
var syncProcessLocalFaults = []string{
	"too many open files",
	"emfile",
	"enfile",
	"secpolicycreatessl",
	"sectrustcreatewithcertificates",
	"seccertificatecreatewithdata",
	"cannot allocate memory",
	"resource temporarily unavailable",
	"database is locked",
}
