package domain

import "testing"

// TestSyncFailureProcessLocal pins the classifier that gates the automatic
// daemon restart. The decisive rows are the two ends: a fault a fresh process
// clears, and one it cannot — and the DEFAULT, which must be "leave it to the
// human" for anything unrecognized.
func TestSyncFailureProcessLocal(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want bool
	}{
		// The observed incident (macOS, 2026-09-09): crypto/x509's darwin
		// verifier reporting a NULL SSL policy. Recurred every tick until the
		// daemon was restarted, and never once after.
		{"macos security framework", `Post "https://db.turso.io/v2/pipeline": tls: failed to verify certificate: SecPolicyCreateSSL error: 0`, true},
		{"descriptor exhaustion", "open /dev/null: too many open files", true},
		{"wedged gate", "database is locked", true},

		// Everything the far end, the link or the credentials are entitled to
		// say. A restart buys nothing here and costs the herd its in-flight
		// captures and consults, every cooldown, forever.
		{"timeout", `Post "https://db.turso.io/v2/pipeline": context deadline exceeded`, false},
		{"dns", "dial tcp: lookup db.turso.io: no such host", false},
		{"refused", "dial tcp 1.2.3.4:443: connect: connection refused", false},
		{"revoked token", "sync engine error: 401 Unauthorized", false},
		{"remote outage", "sync engine error: 503 Service Unavailable", false},

		// The default. An error nobody has classified is left to the operator,
		// who gets the banner either way.
		{"unknown", "sync engine error: something nobody has seen before", false},
		{"empty", "", false},

		// A message carrying BOTH must read as the network fault it is: the
		// remote list is checked first precisely so a TLS error nested inside a
		// dial timeout does not order a pointless restart.
		{"tls inside a timeout", `tls: failed to verify certificate: SecPolicyCreateSSL error: 0: context deadline exceeded`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := SyncFailureProcessLocal(c.msg); got != c.want {
				t.Errorf("SyncFailureProcessLocal(%q) = %v, want %v", c.msg, got, c.want)
			}
		})
	}
}

// TestSyncFailureProcessLocalIgnoresCase: adapters and the Go runtime disagree
// on capitalization, and a classifier that only matched one spelling would be
// silently off for the other.
func TestSyncFailureProcessLocalIgnoresCase(t *testing.T) {
	if !SyncFailureProcessLocal("TLS: SecPolicyCreateSSL ERROR: 0") {
		t.Error("the match must be case-insensitive")
	}
}
