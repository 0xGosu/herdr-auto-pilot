package tui

import (
	"fmt"
	"os"
	"path/filepath"
)

const hapShortcutPath = "/usr/local/bin/hap"

// installHAPShortcut links the exact executable backing this TUI process. It
// deliberately does not use argv[0] or PATH, either of which could identify a
// wrapper or a different hap installation.
func installHAPShortcut() error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate the running hap binary: %w", err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return fmt.Errorf("resolve the running hap binary: %w", err)
	}
	return ensureExecutableSymlink(executable, hapShortcutPath)
}

// ensureExecutableSymlink creates target without replacing anything already
// there. Re-running the shortcut is successful when target already resolves to
// source, while an unrelated file/link is left untouched and reported.
func ensureExecutableSymlink(source, target string) error {
	resolvedSource, err := filepath.EvalSymlinks(source)
	if err != nil {
		return fmt.Errorf("resolve symlink source %s: %w", source, err)
	}
	resolvedSource, err = filepath.Abs(resolvedSource)
	if err != nil {
		return fmt.Errorf("make symlink source absolute: %w", err)
	}

	info, err := os.Lstat(target)
	if err == nil {
		if info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("%s already exists and is not a symlink", target)
		}
		resolvedTarget, resolveErr := filepath.EvalSymlinks(target)
		if resolveErr == nil {
			resolvedTarget, resolveErr = filepath.Abs(resolvedTarget)
		}
		if resolveErr == nil && filepath.Clean(resolvedTarget) == filepath.Clean(resolvedSource) {
			return nil
		}
		return fmt.Errorf("%s already points to a different target; remove it first", target)
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("inspect %s: %w", target, err)
	}
	if err := os.Symlink(resolvedSource, target); err != nil {
		return fmt.Errorf("create %s symlink: %w", target, err)
	}
	return nil
}
