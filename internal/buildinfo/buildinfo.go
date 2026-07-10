// Package buildinfo carries the release version stamped at build time, so
// packages outside main (daemon logs, lock file, status output) can report
// which binary they belong to.
package buildinfo

// Version is stamped by the release build
// (-ldflags "-X github.com/0xGosu/herdr-auto-pilot/internal/buildinfo.Version=...").
var Version = "dev"
