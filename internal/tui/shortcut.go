package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/0xGosu/herdr-auto-pilot/internal/selfpath"
)

const hapShortcutPath = "/usr/local/bin/hap"

// shortcutState describes what the shortcut path currently holds, so the menu
// can say whether the action creates, repoints, or does nothing.
type shortcutState int

const (
	// shortcutAbsent: nothing at the path — the action creates it.
	shortcutAbsent shortcutState = iota
	// shortcutCurrent: already resolves to the running binary.
	shortcutCurrent
	// shortcutStale: a link we recognize as ours, pointing somewhere dead or
	// superseded — the action repoints it.
	shortcutStale
	// shortcutForeign: something we must not touch (a real file, or a link to
	// a live binary that is not one of ours).
	shortcutForeign
)

// installHAPShortcut links the exact executable backing this TUI process. It
// deliberately does not use argv[0], which could identify a wrapper rather
// than the binary itself. (selfpath does consult PATH, but only as a last
// resort once the running binary has been removed — the very case where
// "the executable backing this process" no longer names anything.)
func installHAPShortcut() error {
	executable, err := selfpath.Resolve()
	if err != nil {
		return fmt.Errorf("locate the running hap binary: %w", err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return fmt.Errorf("resolve the running hap binary: %w", err)
	}
	return ensureExecutableSymlink(executable, hapShortcutPath)
}

// hapShortcutState reports what the shortcut path holds relative to the
// running binary, for labelling the menu row.
func hapShortcutState() shortcutState {
	executable, err := selfpath.Resolve()
	if err != nil {
		return shortcutForeign
	}
	state, _ := classifyShortcut(executable, hapShortcutPath)
	return state
}

// shortcutLabel renders the menu row for a shortcut state, so the operator
// knows what pressing enter will do before they press it — an upgrade turns
// "create" into "repoint" without anything else changing on screen.
func shortcutLabel(state shortcutState) string {
	switch state {
	case shortcutCurrent:
		return "Recreate " + hapShortcutPath + " symlink (already points at this binary)"
	case shortcutStale:
		return "Repoint " + hapShortcutPath + " symlink to this running binary (currently stale)"
	default:
		return "Create " + hapShortcutPath + " symlink to this running binary"
	}
}

// shortcutConfirm is the confirmation prompt matching a shortcut state.
func shortcutConfirm(state shortcutState) string {
	if state == shortcutStale {
		return "Repoint " + hapShortcutPath + " to the currently running hap binary? [Y/n]"
	}
	return "Create " + hapShortcutPath + " symlink to the currently running hap binary? [Y/n]"
}

// shortcutResult is the message reported after a successful action.
func shortcutResult(state shortcutState) string {
	if state == shortcutStale {
		return "repointed " + hapShortcutPath + " symlink"
	}
	return "created " + hapShortcutPath + " symlink"
}

// classifyShortcut inspects target and reports what it is relative to source.
// A shortcutForeign result always carries the error explaining why.
func classifyShortcut(source, target string) (shortcutState, error) {
	resolvedSource, err := resolveAbs(source)
	if err != nil {
		return shortcutForeign, fmt.Errorf("resolve symlink source %s: %w", source, err)
	}

	info, err := os.Lstat(target)
	if os.IsNotExist(err) {
		return shortcutAbsent, nil
	}
	if err != nil {
		return shortcutForeign, fmt.Errorf("inspect %s: %w", target, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return shortcutForeign, fmt.Errorf("%s already exists and is not a symlink", target)
	}

	resolvedTarget, err := resolveAbs(target)
	if err != nil {
		// A DANGLING link — what an upgrade leaves behind, since herdr installs
		// each release in its own directory and removes the old one. Refusing
		// here (the old behaviour) left the operator's `hap` command broken
		// with no in-app way to fix it.
		return shortcutStale, nil
	}
	if resolvedTarget == resolvedSource {
		return shortcutCurrent, nil
	}
	// A live binary somewhere else. Supersede it only when it is recognizably
	// a hap from a herdr plugin install (a previous version of ourselves);
	// anything else is the operator's and stays untouched.
	if ownedByHerdrPlugin(resolvedTarget) {
		return shortcutStale, nil
	}
	return shortcutForeign, fmt.Errorf("%s already points to a different target; remove it first", target)
}

// resolveAbs canonicalizes a path through symlinks. Both callers need the
// same two steps, and a failure in either means "cannot be resolved".
func resolveAbs(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	return filepath.Abs(resolved)
}

// ownedByHerdrPlugin reports whether path is a hap binary living inside THIS
// machine's herdr plugin install tree — i.e. a hap we installed, safe to
// supersede. Anchored at the real install root rather than matched as a
// substring, so an unrelated `.../herdr/plugins/...` path elsewhere on disk
// cannot claim to be ours.
func ownedByHerdrPlugin(path string) bool {
	if filepath.Base(path) != "hap" {
		return false
	}
	root := selfpath.PluginInstallRoot()
	if root == "" {
		return false
	}
	// path arrives resolved (the caller EvalSymlinks'd it); resolve the root
	// the same way where it exists, so a symlinked config home still matches.
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// ensureExecutableSymlink points target at source. Re-running is successful
// when target already resolves to source; a stale link left by an upgrade is
// repointed; an unrelated file, or a link to someone else's live binary, is
// left untouched and reported.
func ensureExecutableSymlink(source, target string) error {
	state, err := classifyShortcut(source, target)
	if err != nil {
		return err
	}
	resolvedSource, err := resolveAbs(source)
	if err != nil {
		return fmt.Errorf("resolve symlink source %s: %w", source, err)
	}

	switch state {
	case shortcutCurrent:
		return nil
	case shortcutAbsent:
		if err := os.Symlink(resolvedSource, target); err != nil {
			return fmt.Errorf("create %s symlink: %w", target, err)
		}
		return nil
	case shortcutStale:
		return repointSymlink(resolvedSource, target)
	}
	// classifyShortcut always pairs shortcutForeign with an error, which the
	// guard above returned; keep a message rather than silently succeeding.
	return fmt.Errorf("%s cannot be updated", target)
}

// repointSymlink replaces target atomically, so a concurrent `hap` in another
// shell never observes the path missing. The temp link sits in the same
// directory because rename cannot cross filesystems.
func repointSymlink(source, target string) error {
	tmp := target + ".hap-tmp"
	// A leftover temp link from an interrupted repoint would fail Symlink.
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clear stale temp link %s: %w", tmp, err)
	}
	if err := os.Symlink(source, tmp); err != nil {
		return fmt.Errorf("create replacement symlink for %s: %w", target, err)
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("repoint %s: %w", target, err)
	}
	return nil
}
