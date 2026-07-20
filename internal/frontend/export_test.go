package frontend

// CwdTTLForTest exposes the working-directory cache TTL to the external test
// package so expiry can be exercised at the real boundary instead of a
// hard-coded duplicate that would silently drift from it.
const CwdTTLForTest = cwdTTL
