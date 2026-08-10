package tasklocator_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/tasklocator"
)

// TestDisplayKeepsTheSymlinkWhileIdentityResolvesIt reproduces the macOS
// condition locally: /var is a symlink to /private/var there, so a display
// address built by resolving symlinks reads back as a path the operator never
// wrote. Display must keep their spelling; the LOCATOR must still collapse
// both spellings to one identity.
func TestDisplayKeepsTheSymlinkWhileIdentityResolvesIt(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	viaLink := filepath.Join(link, "tasks.md")
	if err := os.WriteFile(filepath.Join(real, "tasks.md"), []byte("- [ ] a\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := tasklocator.DisplayPath(viaLink); got != viaLink {
		t.Errorf("DisplayPath(%q) = %q — an operator must be able to match what hap "+
			"printed against the path they configured", viaLink, got)
	}
	// Identity still collapses both spellings, which is what shares one lock.
	if tasklocator.Canonical(viaLink) != tasklocator.Canonical(filepath.Join(real, "tasks.md")) {
		t.Error("the locator must resolve both spellings to one identity")
	}
}
