package config

import (
	"os"
	"path/filepath"
	"strings"
)

// ExpandPath resolves a user-configured filesystem path so operators can write
// the shorthands they expect in config.toml. It performs two substitutions, in
// order:
//
//  1. Environment variables: `$VAR` and `${VAR}` are replaced with their values
//     via os.ExpandEnv (an undefined variable expands to "", matching the shell).
//     A consequence, also matching the shell, is that a literal `$` in a path
//     (e.g. a file named `cost$5.md`) is treated as a variable reference — keep
//     `$` out of paths that are not meant to interpolate.
//  2. Home directory: a leading `~` (alone) or `~/…` is replaced with the current
//     user's home directory. A `~` anywhere but the start (e.g. `/opt/~/x`), and
//     the `~user` form, are left literal.
//
// A path with neither `$` nor a leading `~` is returned unchanged, so absolute
// and plain relative paths pass through untouched — making a relative path
// absolute is the caller's job (e.g. filepath.Abs), deliberately kept separate.
//
// Expansion is best-effort: when the home directory cannot be determined, the
// leading `~` is left literal rather than erroring, so a misconfigured
// environment degrades to the written path instead of failing the caller. This
// is the shared helper for path-valued config keys: task_sources.path,
// embedding.model_path, and the five [llm] *_env_file keys.
func ExpandPath(path string) string {
	if path == "" {
		return ""
	}
	path = os.ExpandEnv(path)
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, path[2:])
}
