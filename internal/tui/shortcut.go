package tui

import (
	"path/filepath"

	"github.com/0xGosu/herdr-auto-pilot/internal/shortcut"
)

// The Quick Shortcuts row for the operator's `hap` command. Where the link
// lives, whose it is, and how it is repointed are internal/shortcut's — shared
// with `hap link` and `hap update` — so this file only words the row.

// installHAPShortcut links the executable backing this TUI process.
func installHAPShortcut() error {
	_, err := shortcut.Install()
	return err
}

// shortcutLabel renders the menu row for a located shortcut, so the operator
// knows what pressing enter will do — and to which path — before they press
// it: an upgrade turns "create" into "repoint" without anything else changing
// on screen.
func shortcutLabel(link shortcut.Link) string {
	switch link.State {
	case shortcut.Current:
		return "Recreate " + link.Path + " symlink (already points at this binary)"
	case shortcut.Stale:
		return "Repoint " + link.Path + " symlink to this running binary (currently stale)"
	default:
		return "Create " + link.Path + " symlink to this running binary"
	}
}

// shortcutConfirm is the confirmation prompt matching a located shortcut.
func shortcutConfirm(link shortcut.Link) string {
	if link.State == shortcut.Stale {
		return "Repoint " + link.Path + " to the currently running hap binary? [Y/n]"
	}
	return "Create " + link.Path + " symlink to the currently running hap binary? [Y/n]"
}

// shortcutResult is the message reported after a successful action. A link
// outside PATH is a success that still leaves `hap` unfound, so it says so.
func shortcutResult(link shortcut.Link) string {
	msg := "created " + link.Path + " symlink"
	if link.State == shortcut.Stale {
		msg = "repointed " + link.Path + " symlink"
	}
	if !link.OnPath {
		msg += " — add " + filepath.Dir(link.Path) + " to your PATH to use it"
	}
	return msg
}
