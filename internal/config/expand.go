package config

import (
	"os"
	"path/filepath"
	"strings"
)

// ExpandPath resolves a user-configured filesystem path so operators can write
// the shorthands they expect in config.toml. It performs two substitutions:
//
//  1. Environment variables: `$VAR` and `${VAR}` are replaced with their values
//     via os.ExpandEnv (an undefined variable expands to "", as in the shell).
//     A consequence, also as in the shell, is that a literal `$` in a path
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
// The two steps are iterated to a fixed point, which makes ExpandPath
// idempotent for any terminating chain: ExpandPath(ExpandPath(x)) ==
// ExpandPath(x). This matters because the helper is applied at several layers
// (e.g. a task path is expanded for its lock key AND for the read/write), and
// os.ExpandEnv is single-pass — a value that expands to another `$VAR` (e.g.
// `A=$B`, `B=/tmp/x`) would otherwise be left partially expanded, so a second
// pass would change it again and the lock key would diverge from the file
// actually read. The iteration is therefore deliberately MORE aggressive than
// the shell (which does not re-expand a value that itself contains `$`); the
// goal is cross-layer idempotency, not shell fidelity. The loop is capped, so a
// reference CYCLE (`A=$B`, `B=$A`) terminates by returning a bounded, partially
// expanded value instead of spinning — that value is not a fixed point, but a
// path built from a variable cycle never names a real file, so the identity it
// would otherwise break is moot.
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
	// A shallow cap: real $VAR/~ chains are one or two levels deep; the bound
	// only guards a pathological mutual reference.
	for i := 0; i < 8; i++ {
		next := expandPathOnce(path)
		if next == path {
			break
		}
		path = next
	}
	return path
}

// expandPathOnce applies one round of env-var then leading-~ substitution.
func expandPathOnce(path string) string {
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
