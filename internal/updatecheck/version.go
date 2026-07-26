// Package updatecheck tells an operator that a newer hap release exists.
//
// It is split so the network stays in one place: version.go and cache.go are
// pure logic plus local file I/O, and fetch.go is the ONLY file in the plugin
// permitted to import net/http (see internal/privacy — NFR-007 bans egress
// everywhere else). Every failure mode here is non-fatal: a check that cannot
// run simply leaves the header without a hint.
package updatecheck

import (
	"strconv"
	"strings"
)

// parse splits a release version into its numeric components. It accepts an
// optional "v" prefix and ignores any pre-release/build suffix, so both the
// release tag ("v0.5.2") and a bare manifest version ("0.5.2") parse. ok is
// false for anything that is not a release version — notably the "dev" and
// "dev-<timestamp>" values local builds are stamped with.
func parse(v string) (nums [3]int, ok bool) {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	if v == "" {
		return nums, false
	}
	// Drop a pre-release/build suffix: 1.2.3-rc1 and 1.2.3+meta compare as 1.2.3.
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return nums, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nums, false
		}
		nums[i] = n
	}
	return nums, true
}

// IsRelease reports whether v names a release version (and so can be compared).
func IsRelease(v string) bool {
	_, ok := parse(v)
	return ok
}

// Compare orders two release versions: -1 if a < b, 0 if equal, 1 if a > b.
// A value that is not a release version sorts below one that is; two
// unparseable values compare equal, which keeps callers from acting on them.
func Compare(a, b string) int {
	na, oka := parse(a)
	nb, okb := parse(b)
	switch {
	case !oka && !okb:
		return 0
	case !oka:
		return -1
	case !okb:
		return 1
	}
	for i := range na {
		switch {
		case na[i] < nb[i]:
			return -1
		case na[i] > nb[i]:
			return 1
		}
	}
	return 0
}

// IsNewer reports whether latest is a release strictly newer than current.
// It is false whenever current is not itself a release version, so a linked
// working-tree build ("dev", "dev-<timestamp>") never nags its developer.
func IsNewer(current, latest string) bool {
	if !IsRelease(current) || !IsRelease(latest) {
		return false
	}
	return Compare(latest, current) > 0
}
