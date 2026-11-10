package tui

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/0xGosu/herdr-auto-pilot/internal/shortcut"
)

// The Quick Shortcuts row for the operator's `hap` command. Where the link
// lives, whose it is, and how it is repointed are internal/shortcut's — shared
// with `hap link` and `hap update` — so this file only words the row.
//
// Every string here is shown on ONE line that the pane truncates, so each
// puts what the operator must act on first and spells paths with ~: the
// original failure report for this row read "create replacement symlink for
// /usr/local/bin/hap: symlink /Users…" — the reason was past the cut.

// installHAPShortcut links the executable backing this TUI process.
func installHAPShortcut() error {
	_, err := shortcut.Install()
	return err
}

// tildePath abbreviates the operator's home directory, which is most of the
// width of a per-user path.
func tildePath(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || home == string(filepath.Separator) {
		return path
	}
	if path == home {
		return "~"
	}
	if rest, ok := strings.CutPrefix(path, home+string(filepath.Separator)); ok {
		return "~" + string(filepath.Separator) + rest
	}
	return path
}

// shortcutLabel renders the menu row for a located shortcut, so the operator
// knows what pressing enter will do — and to which path — before they press
// it: an upgrade turns "create" into "repoint" without anything else changing
// on screen.
func shortcutLabel(link shortcut.Link) string {
	path := tildePath(link.Path)
	switch {
	case link.State == shortcut.Current:
		return "Recreate " + path + " symlink (already points at this binary)"
	case link.State == shortcut.Stale:
		return "Repoint " + path + " symlink to this running binary (currently stale)"
	case link.Blocked != "":
		// The stale link is where the operator expects the row to act; say
		// why it acts somewhere else instead.
		return "Create " + path + " symlink to this running binary (" + tildePath(link.Blocked) + " is stale and needs sudo)"
	default:
		return "Create " + path + " symlink to this running binary"
	}
}

// shortcutConfirm is the confirmation prompt matching a located shortcut.
func shortcutConfirm(link shortcut.Link) string {
	if link.State == shortcut.Stale {
		return "Repoint " + tildePath(link.Path) + " to the currently running hap binary? [Y/n]"
	}
	return "Create " + tildePath(link.Path) + " symlink to the currently running hap binary? [Y/n]"
}

// shortcutResult is the message reported after a successful action. Two
// successes still leave `hap` not doing what the operator expects, and each
// says what is left to do: a link outside PATH is not found, and a stale link
// that could not be rewritten keeps shadowing the new one for as long as it
// resolves.
func shortcutResult(link shortcut.Link) string {
	msg := "created " + tildePath(link.Path) + " symlink"
	if link.State == shortcut.Stale {
		msg = "repointed " + tildePath(link.Path) + " symlink"
	}
	var todo []string
	// Only when the old link really wins in a shell. Where it comes later on
	// PATH (or is dead) it is litter, and a one-line result has no room to
	// send the operator after litter with sudo.
	if link.Shadows {
		todo = append(todo, "run: sudo rm "+link.Blocked+" (stale, shadows it)")
	}
	if !link.OnPath {
		todo = append(todo, "add "+tildePath(filepath.Dir(link.Path))+" to your PATH")
	}
	if len(todo) > 0 {
		msg += " — " + strings.Join(todo, "; ")
	}
	return msg
}
