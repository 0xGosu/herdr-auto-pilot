// Package testutil holds shared test helpers.
package testutil

import (
	"os"
	"testing"
)

// SocketDir returns a directory with a short absolute path for unix-domain
// sockets. macOS caps socket paths at 104 bytes, and t.TempDir() embeds the
// full test name — long enough to overflow the cap and fail net.Listen.
func SocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "hap")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}
