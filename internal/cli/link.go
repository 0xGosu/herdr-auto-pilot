package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/shortcut"
)

// installShortcut and healShortcut change the operator's `hap` link. Vars so
// tests describe an outcome without a link on disk; the real ones refuse to
// run inside a test binary at all (shortcut.refuseUnderTest), which is what
// keeps the registry sweep in hints_test.go off the operator's PATH.
var (
	installShortcut = shortcut.Install
	healShortcut    = shortcut.Heal
)

// link implements `hap link`: the CLI twin of the TUI's Quick Shortcuts row.
// It exists for the state the row cannot reach — after a plugin update the
// old link is dangling, so `hap` is "command not found" and the only hap an
// operator can still run is the new binary by its full path.
func link(_ context.Context, _ *frontend.App, out io.Writer, args []string) error {
	if len(args) > 0 {
		return errors.New("usage: hap link")
	}
	l, err := installShortcut()
	if err != nil {
		return err
	}
	switch l.State {
	case shortcut.Current:
		fmt.Fprintf(out, "%s already points at this binary (%s)\n", l.Path, l.Source)
	case shortcut.Stale:
		fmt.Fprintf(out, "repointed %s -> %s\n", l.Path, l.Source)
	default:
		fmt.Fprintf(out, "created %s -> %s\n", l.Path, l.Source)
	}
	if !l.OnPath {
		fmt.Fprintf(out, "note: %s is not on your PATH — add it, or `hap` still will not be found\n", filepath.Dir(l.Path))
	}
	return nil
}

// repointShortcut moves the operator's `hap` link to the binary an install
// just left behind. herdr puts every install in a new directory and removes
// the old one, so without this the link dangles the moment `hap update`
// finishes and the very next `hap` is "command not found".
//
// It is unattended, so it only ever repoints a link that was already hap's
// (see shortcut.Heal) and never creates one. A failure is a note, not an
// error: the install itself succeeded.
func repointShortcut(out io.Writer, newBin string) {
	if newBin == "" {
		return
	}
	path, repointed, err := healShortcut(newBin)
	switch {
	case path == "":
		// No link of ours (or a test binary, where Heal refuses): nothing to say.
	case err != nil:
		fmt.Fprintf(out, "note: could not repoint %s at the new build (%v) — run `%s link`\n", path, err, newBin)
	case repointed:
		fmt.Fprintf(out, "repointed %s at the new build\n", path)
	}
}
