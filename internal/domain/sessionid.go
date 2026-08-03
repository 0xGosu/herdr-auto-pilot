package domain

import (
	"crypto/rand"
	"encoding/hex"
)

// NewSessionID mints the identifier for one LLM CLI invocation: a random
// (version 4) UUID.
//
// A UUID specifically, and not the existing RequestID, because `claude
// --session-id` REJECTS anything that is not one — RequestID is
// "req-<agent>-<nanos>". The two name different things and both are kept:
// RequestID names hap's own staged row (and rides in HAP_REQUEST_ID), while
// this names the CLI's conversation and is what its transcript file is called.
//
// Hand-rolled rather than pulling in a UUID library because this package is
// stdlib-only by rule (TestDomainPurity) — and 16 random bytes with two fixed
// nibbles is the whole of RFC 4122 §4.4.
//
// Returns "" if the system entropy source fails. That is not an error worth
// propagating: a session id is bookkeeping, and no consult should fail because
// hap could not name it. Every consumer already treats "" as "unknown".
func NewSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:36], b[10:16])
	return string(out[:])
}
