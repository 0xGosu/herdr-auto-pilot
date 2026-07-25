// Package selfpath resolves a path to a LIVE hap executable.
//
// Every self-spawn in hap — the {self} placeholder handed to the operator's
// LLM CLI so it can launch `hap mcp`, the embed-worker subprocess, the daemon
// re-exec — needs to name this binary on disk. os.Executable() alone is not
// enough: herdr installs each plugin release into its own directory
// (~/.config/herdr/plugins/<source>/herd-auto-prompter-*/bin/hap) and an
// upgrade unlinks the old one, so a long-lived process keeps reporting a path
// that no longer exists (Go strips the " (deleted)" suffix procfs appends).
// Every child spawned from that path then fails with ENOENT, silently: the LLM
// CLI cannot start the MCP server, the embedder latches degraded mode.
//
// Resolve therefore VALIDATES os.Executable's answer and, only when it has gone
// away, looks for the binary that replaced it.
package selfpath

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// EnvOverride names the environment variable that pins the resolved path. It
// is both the test seam and the escape hatch for installs this package cannot
// discover on its own.
const EnvOverride = "HAP_SELF_PATH"

// pluginID is the herdr plugin id; it names both the registry entry and the
// install directory prefix.
const pluginID = "herd-auto-prompter"

// herdrLookupTimeout bounds the `herdr plugin list` fallback so a wedged herdr
// cannot stall a spawn. It only runs once the running executable has vanished.
const herdrLookupTimeout = 5 * time.Second

// ErrNotFound reports that no live hap executable could be located.
var ErrNotFound = errors.New("no live hap executable found")

var (
	cacheMu sync.Mutex
	// cached holds the last successful FALLBACK answer (never the fast-path
	// one, which is already free to compute). It is re-validated on every use
	// so a second upgrade is picked up too.
	cached string
)

// Resolve returns the path of a live hap executable, preferring the running
// one. Callers use it for re-exec and for the {self} placeholder.
func Resolve() (string, error) {
	if p := os.Getenv(EnvOverride); p != "" {
		// An explicit override is authoritative: report it as-is rather than
		// silently substituting something else, so a wrong value is visible.
		return p, nil
	}
	// Fast path: the running binary, if it is still on disk. One Stat, and it
	// is the answer in every case except an upgrade.
	if exe, err := os.Executable(); err == nil && executable(exe) {
		return exe, nil
	}
	return resolveReplacement()
}

// PluginRoot locates the plugin install dir from the resolved binary:
// install.sh places it at <root>/bin/hap, so root is two levels up. Falls back
// to the working directory when nothing can be resolved.
func PluginRoot() string {
	exe, err := Resolve()
	if err != nil {
		return "."
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Dir(filepath.Dir(exe))
}

// Missing reports whether path no longer names an executable file. The daemon
// polls this against the path it started from to notice its own replacement.
func Missing(path string) bool {
	return path != "" && !executable(path)
}

// Same reports whether two paths name the same file once symlinks are
// resolved, so /usr/local/bin/hap and <plugin>/bin/hap do not read as
// different binaries. Two paths that are literally equal are the same even
// when neither can be resolved — a removed binary is still itself. Beyond
// that, an unresolvable path compares false: nothing can be concluded about
// a file that is not there.
func Same(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, err := canonical(a)
	if err != nil {
		return false
	}
	rb, err := canonical(b)
	if err != nil {
		return false
	}
	return ra == rb
}

func canonical(p string) (string, error) {
	if p == "" {
		return "", ErrNotFound
	}
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", err
	}
	return filepath.Abs(resolved)
}

// executable reports whether path is a regular file with an execute bit.
func executable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	return info.Mode().Perm()&0o111 != 0
}

// resolveReplacement finds the binary that replaced a vanished one. Ordered
// most to least authoritative; each candidate must exist to be accepted.
func resolveReplacement() (string, error) {
	cacheMu.Lock()
	prev := cached
	cacheMu.Unlock()
	if executable(prev) {
		return prev, nil
	}

	for _, candidate := range []struct {
		how  string
		find func() string
	}{
		{"herdr plugin registry", fromHerdrRegistry},
		{"herdr plugin install dir", fromInstallDirs},
		{"PATH", fromPATH},
	} {
		if p := candidate.find(); executable(p) {
			cacheMu.Lock()
			cached = p
			cacheMu.Unlock()
			// The result is EXECUTED (as the successor daemon, as the embed
			// worker, and as the MCP server the operator's LLM CLI launches),
			// so which fallback picked it is worth an operator-visible record
			// rather than a silent substitution.
			slog.Warn("the running hap binary is gone; resolved a replacement",
				"replacement", p, "found_via", candidate.how)
			return p, nil
		}
	}
	return "", fmt.Errorf("%w (the running binary was removed, likely by a plugin upgrade)", ErrNotFound)
}

// fromHerdrRegistry asks herdr where the plugin is installed now. This is the
// authoritative answer after an upgrade, because herdr rewrote the registry
// itself.
func fromHerdrRegistry() string {
	bin := os.Getenv("HERDR_BIN_PATH")
	if bin == "" {
		bin = "herdr"
	}
	ctx, cancel := context.WithTimeout(context.Background(), herdrLookupTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "plugin", "list", "--json")
	// A deleted cwd would make the spawn itself fail; the daemon already runs
	// from a stable dir, but this path is reachable from short-lived CLIs too.
	cmd.Dir = stableDir()
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	var env struct {
		Result struct {
			Plugins []struct {
				PluginID   string `json:"plugin_id"`
				PluginRoot string `json:"plugin_root"`
			} `json:"plugins"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &env); err != nil {
		return ""
	}
	for _, p := range env.Result.Plugins {
		if p.PluginID == pluginID && p.PluginRoot != "" {
			return filepath.Join(p.PluginRoot, "bin", "hap")
		}
	}
	return ""
}

// fromInstallDirs scans herdr's plugin install tree directly, for when herdr
// itself cannot be reached. Install dirs are version-stamped, so the newest
// one wins.
func fromInstallDirs() string {
	root := PluginInstallRoot()
	if root == "" {
		return ""
	}
	matches, err := filepath.Glob(filepath.Join(root, "*", pluginID+"*", "bin", "hap"))
	if err != nil {
		return ""
	}
	var newest string
	var newestMod time.Time
	for _, m := range matches {
		info, err := os.Stat(m)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if newest == "" || info.ModTime().After(newestMod) {
			newest, newestMod = m, info.ModTime()
		}
	}
	return newest
}

// fromPATH is the last resort: the operator's own `hap` shortcut
// (/usr/local/bin/hap), which is how a linked working tree or an install this
// package cannot otherwise discover is found.
//
// It is last for a reason. Whatever it returns gets EXECUTED, and PATH is
// inherited from whoever launched the daemon, so a `hap` earlier in PATH than
// the operator's own would be run. The exposure is bounded by ordering (the
// two authoritative sources are tried first, and this is reached only once
// the running binary has been removed) and by the resolution being logged;
// anyone who can write an earlier PATH entry for this process can already
// choose what it runs. Do not promote this above the herdr-anchored lookups.
func fromPATH() string {
	p, err := exec.LookPath("hap")
	if err != nil {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return ""
	}
	return resolved
}

// PluginInstallRoot returns the directory herdr installs plugins under
// (<config home>/herdr/plugins), or "" when the config home cannot be
// resolved. Callers use it to tell a hap WE installed from an unrelated
// binary that happens to be named hap.
func PluginInstallRoot() string {
	home := configHome()
	if home == "" {
		return ""
	}
	return filepath.Join(home, "herdr", "plugins")
}

func configHome() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config")
}

// stableDir picks a directory that is certain to exist, so spawning a helper
// never fails merely because the inherited cwd was deleted.
func stableDir() string {
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	if home, err := os.UserHomeDir(); err == nil {
		return home
	}
	return string(filepath.Separator)
}
