// Package buildinfo carries the release version stamped at build time, so
// packages outside main (daemon logs, lock file, status output) can report
// which binary they belong to.
package buildinfo

import "strings"

// Version is stamped by the release build
// (-ldflags "-X github.com/0xGosu/herdr-auto-pilot/internal/buildinfo.Version=...").
var Version = "dev"

// Label renders Version for display (the TUI header).
func Label() string { return LabelOf(Version) }

// VersionLine is the one-line `hap --version` output. It is a contract, not a
// convenience: `hap update` executes the NEWLY INSTALLED binary's --version
// and parses the version out of this exact shape (cli.versionFromOutput, with
// a round-trip test), so a release that changed the line would silently cost
// every operator updating to it the "installed vX" report. Keep the version
// the last whitespace-separated field.
func VersionLine() string { return "hap (herd-auto-prompter) " + Version }

// LabelOf gives a version string its customary "v" prefix ("0.5.2" → "v0.5.2").
// A value that is already prefixed, or that is not a release version at all
// (the "dev" default), is shown verbatim so a local build never reads "vdev".
// An empty value renders as empty so the caller can omit it entirely; a
// daemon's recorded lock-file version instead goes through
// daemonlock.VersionLabel, which reports the empty case rather than hiding it.
func LabelOf(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if v[0] >= '0' && v[0] <= '9' {
		return "v" + v
	}
	return v
}
