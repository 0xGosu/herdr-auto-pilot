// Package shortcut manages the operator's `hap` command: a symlink in a PATH
// directory pointing at the plugin binary herdr runs.
//
// herdr invokes the plugin as ./bin/hap relative to its install root and puts
// nothing on PATH, so typing `hap` in a shell works only through this link.
// The link is also fragile by construction: herdr installs every update into
// a NEW directory (herdr ≥0.9.5:
// plugins/github-installations/<id>-<hash>/<generation>/checkout; earlier:
// plugins/github/<id>-<hash>, reinstalled in place), so an update strands the link on the
// previous install. Before 0.9.5 that install was removed at once and `hap`
// read "command not found". From 0.9.5 it is RETAINED until the herdr server
// exits, which is worse: `hap` keeps working as the OLD build, silently, and
// only later goes dangling. Both are Stale here — a live link into the plugin
// tree that is not the binary asked about, or a dead one.
//
// Two things used to be hardcoded and both broke on a per-user herdr install:
// the link lived at /usr/local/bin/hap only — root-owned for an operator whose
// herdr sits in ~/.local/bin, so the create failed with EACCES — and a link
// the operator had made anywhere else was invisible, so the TUI offered to
// "create" a second one instead of repointing the dead one. Path therefore
// FINDS an existing link before it proposes a location.
//
// The TUI's Quick Shortcuts row, `hap link` and `hap update` all go through
// here, so the three cannot disagree about where the link is or whose it is.
package shortcut

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/selfpath"
)

// name is the command the link provides.
const name = "hap"

// systemBinDir is the historical home of the link, still preferred when it is
// writable (a root devcontainer). A var so tests never probe the real one.
var systemBinDir = "/usr/local/bin"

// allowUnderTest lets this package's own tests, which isolate PATH and HOME,
// reach the mutating entry points. See refuseUnderTest.
var allowUnderTest bool

// State describes what a shortcut path holds relative to a hap binary, so the
// caller can say whether acting creates, repoints, or does nothing.
type State int

const (
	// Absent: nothing at the path — acting creates it.
	Absent State = iota
	// Current: already resolves to the binary.
	Current
	// Stale: a link recognized as hap's, pointing somewhere dead or
	// superseded — acting repoints it.
	Stale
	// Foreign: something that must not be touched (a real file, or a link to
	// a live binary that is not one of hap's installs).
	Foreign
)

// Link is the shortcut as located for one hap binary.
type Link struct {
	// Path is where the link is, or where it would be created.
	Path string
	// Source is the binary the link is for, symlinks resolved where possible.
	Source string
	// State is what Path holds relative to the binary.
	State State
	// OnPath reports whether Path's directory is in this process's PATH. A
	// link outside PATH is created all the same, but the operator has to be
	// told, or `hap` still reads "command not found".
	OnPath bool
	// Blocked is a stale hap link in a directory this user cannot write —
	// typically /usr/local/bin/hap made with sudo, or made back when the
	// directory was writable. It cannot be repointed from here, so Path is
	// somewhere else; but while Blocked still resolves and comes first on
	// PATH it SHADOWS the new link, so the operator has to be told to remove
	// it (the one step that needs root).
	Blocked string
	// Shadows reports whether Blocked actually wins over Path in a shell:
	// it still resolves, and its directory comes earlier on PATH (or Path's
	// is not on PATH at all). When false Blocked is only litter — as on a
	// machine whose ~/.local/bin precedes /usr/local/bin — and the operator
	// has nothing they must do.
	Shadows bool
}

// Locate reports the shortcut for the running binary, read-only. It is what
// the TUI row is labelled from.
func Locate() Link {
	source, err := selfpath.Resolve()
	if err != nil {
		return Link{Path: Path(""), State: Foreign}
	}
	return locate(source)
}

func locate(source string) Link {
	path, blocked := find(source)
	state, _ := Classify(source, path)
	return Link{
		Path: path, Source: source, State: state, OnPath: onPath(filepath.Dir(path)),
		Blocked: blocked, Shadows: shadows(blocked, path),
	}
}

// shadows reports whether the link at blocked is what a shell runs for `hap`
// in preference to the one at path. A dangling link never is: a shell's PATH
// search skips a name it cannot execute.
func shadows(blocked, path string) bool {
	if blocked == "" {
		return false
	}
	if _, err := os.Stat(blocked); err != nil {
		return false
	}
	theirs, ours := pathIndex(filepath.Dir(blocked)), pathIndex(filepath.Dir(path))
	return theirs >= 0 && (ours < 0 || theirs < ours)
}

// pathIndex is dir's position in PATH, or -1.
func pathIndex(dir string) int {
	dir = filepath.Clean(dir)
	for i, entry := range filepath.SplitList(os.Getenv("PATH")) {
		if filepath.IsAbs(entry) && filepath.Clean(entry) == dir {
			return i
		}
	}
	return -1
}

// Install links the executable backing this process and reports the link as
// it was BEFORE the change, so the caller can say "created" or "repointed".
// It deliberately does not use argv[0], which could identify a wrapper rather
// than the binary itself. (selfpath does consult PATH, but only as a last
// resort once the running binary has been removed — the very case where "the
// executable backing this process" no longer names anything.)
func Install() (Link, error) {
	if err := refuseUnderTest(); err != nil {
		return Link{}, err
	}
	source, err := selfpath.Resolve()
	if err != nil {
		return Link{}, fmt.Errorf("locate the running hap binary: %w", err)
	}
	// herdr ≥0.9.5 keeps a replaced install runnable, so this process may be
	// a SUPERSEDED build (a TUI left open across an update, a `hap link`
	// typed through a stale path). To it, a link `hap update` already moved
	// to the new install reads as Stale — "a previous version of ourselves" —
	// and repointing would move the operator's `hap` BACK to the old build.
	// Link the install herdr runs instead, as `daemon --ensure` does.
	if registered := selfpath.SupersededBy(source); registered != "" {
		source = registered
	}
	return install(source)
}

func install(source string) (Link, error) {
	resolved, err := resolveAbs(source)
	if err != nil {
		return Link{}, fmt.Errorf("resolve the running hap binary: %w", err)
	}
	link := locate(resolved)
	return link, Ensure(resolved, link.Path)
}

// Heal repoints a shortcut that a plugin update left behind at source, the
// newly installed binary. It is the unattended counterpart of Install —
// `hap update` calls it with no confirmation — so it is narrower on both
// sides: it never CREATES a link (an operator who has none did not ask for
// one), and it only touches a link whose own target is, or was, inside
// herdr's plugin install tree. A dangling link to anywhere else is left for
// the operator to repoint deliberately.
//
// It returns the link it found ("" for none) and whether it was repointed.
func Heal(source string) (string, bool, error) {
	if err := refuseUnderTest(); err != nil {
		return "", false, err
	}
	resolved, err := resolveAbs(source)
	if err != nil {
		return "", false, fmt.Errorf("resolve the installed hap binary %s: %w", source, err)
	}
	var blocked string
	for _, dir := range searchDirs() {
		path := filepath.Join(dir, name)
		if !isSymlink(path) {
			continue
		}
		switch state, _ := Classify(resolved, path); state {
		case Current:
			return path, false, nil
		case Stale:
			if !pointsIntoPluginTree(path) {
				continue
			}
			// A link that cannot be rewritten is not the end of the search:
			// the operator may have a second, user-level one further along
			// (which is exactly what `hap link` makes in this situation).
			if !writableDir(dir) {
				if blocked == "" {
					blocked = path
				}
				continue
			}
			if err := repointSymlink(resolved, path); err != nil {
				return path, false, err
			}
			return path, true, nil
		}
	}
	if blocked != "" {
		return blocked, false, fmt.Errorf("%s is not writable", filepath.Dir(blocked))
	}
	return "", false, nil
}

// errUnderTest is returned by the mutating entry points inside a test binary.
var errUnderTest = errors.New("refusing to change the hap shortcut from a test")

// refuseUnderTest is the structural guarantee that no test — in any package —
// rewrites the operator's real `hap` link: the CLI registry is swept by tests
// that invoke every handler, and `hap update`'s tests run the whole verb with
// only the install faked. Same shape as cli.runHerdrInstall's refusal.
func refuseUnderTest() error {
	if testing.Testing() && !allowUnderTest {
		return errUnderTest
	}
	return nil
}

// Path returns where the shortcut for source is: an existing link of ours if
// one can be found, otherwise the place a new one belongs.
//
// The search covers every PATH directory (in PATH order, which is the order a
// shell consults them) and then the two well-known homes, because a TUI pane
// inherits herdr's PATH rather than the operator's login shell's. A regular
// file named hap, or a link to someone else's live binary, is skipped rather
// than claimed.
//
// A stale link in a directory this user cannot write is not returned either:
// it can only be repointed as root. Offering it anyway is how the TUI row came
// to fail forever with "permission denied" on a machine whose
// /usr/local/bin/hap was once made with sudo — the very failure this package
// exists to remove. See find for what happens instead.
func Path(source string) string {
	path, _ := find(source)
	return path
}

// find is Path plus the stale link it had to step over (Link.Blocked).
//
// A Current link is returned wherever it is — there is nothing to write. A
// Stale one is returned only from a writable directory. The first Stale link
// found in an UNWRITABLE directory is reported as blocked, and the search
// goes on: to a writable link of ours further along, else to the default
// location, where a new one is created.
func find(source string) (path, blocked string) {
	for _, dir := range searchDirs() {
		candidate := filepath.Join(dir, name)
		if !isSymlink(candidate) {
			continue
		}
		switch state, _ := Classify(source, candidate); state {
		case Current:
			return candidate, blocked
		case Stale:
			if writableDir(dir) {
				return candidate, blocked
			}
			if blocked == "" {
				blocked = candidate
			}
		}
	}
	return filepath.Join(defaultDir(), name), blocked
}

// defaultDir picks where a NEW link goes, preferring a directory that is both
// writable and already on PATH, so the link works with no further step:
//
//  1. the system directory (the historical location, on every shell's PATH)
//     when this user can write it — a root devcontainer;
//  2. the directory `herdr` itself is run from: the operator installed a
//     command there and runs it, so it is theirs and on their PATH
//     (~/.local/bin for herdr's own installer, /opt/homebrew/bin for brew).
//     macOS has no ~/.local/bin on PATH by default, which is what makes this
//     step matter there;
//  3. ~/.local/bin, created if missing, with the PATH note when needed.
//
// With no home directory to fall back to it stays the system directory, so
// the failure names a real path.
func defaultDir() string {
	if writableDir(systemBinDir) {
		return systemBinDir
	}
	if dir := herdrBinDir(); dir != "" {
		return dir
	}
	if user := userBinDir(); user != "" {
		return user
	}
	return systemBinDir
}

// herdrBinDir returns the PATH directory herdr is run from, when this user
// can write it. HERDR_BIN_PATH is tried first because herdr sets it for every
// plugin command, but it may name a resolved, version-stamped location (a
// Homebrew Cellar path) rather than the directory on PATH — so each candidate
// must itself be a PATH entry, and the PATH lookup follows.
func herdrBinDir() string {
	var candidates []string
	if bin := os.Getenv("HERDR_BIN_PATH"); filepath.IsAbs(bin) {
		candidates = append(candidates, filepath.Dir(bin))
	}
	if bin, err := exec.LookPath("herdr"); err == nil && filepath.IsAbs(bin) {
		candidates = append(candidates, filepath.Dir(bin))
	}
	for _, dir := range candidates {
		if onPath(dir) && writableDir(dir) {
			return filepath.Clean(dir)
		}
	}
	return ""
}

func userBinDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".local", "bin")
}

// searchDirs lists the directories a hap link may live in, most significant
// first, without duplicates. Relative PATH entries are ignored: they name a
// different directory for every process.
func searchDirs() []string {
	var dirs []string
	seen := map[string]bool{}
	add := func(dir string) {
		if dir == "" || !filepath.IsAbs(dir) {
			return
		}
		dir = filepath.Clean(dir)
		if !seen[dir] {
			seen[dir] = true
			dirs = append(dirs, dir)
		}
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		add(dir)
	}
	add(systemBinDir)
	add(userBinDir())
	return dirs
}

func onPath(dir string) bool { return pathIndex(dir) >= 0 }

// writableDir reports whether this process may create entries in dir. It asks
// the kernel (access(2), W_OK) rather than probing with a temp file: the TUI
// relabels its row on every refresh.
func writableDir(dir string) bool {
	const wOK = 0x2
	info, err := os.Stat(dir)
	return err == nil && info.IsDir() && syscall.Access(dir, wOK) == nil
}

func isSymlink(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode()&os.ModeSymlink != 0
}

// Classify inspects target and reports what it is relative to source. A
// Foreign result always carries the error explaining why.
func Classify(source, target string) (State, error) {
	resolvedSource, err := resolveAbs(source)
	if err != nil {
		return Foreign, fmt.Errorf("resolve symlink source %s: %w", source, err)
	}

	info, err := os.Lstat(target)
	if os.IsNotExist(err) {
		return Absent, nil
	}
	if err != nil {
		return Foreign, fmt.Errorf("inspect %s: %w", target, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return Foreign, fmt.Errorf("%s already exists and is not a symlink", target)
	}

	resolvedTarget, err := resolveAbs(target)
	if err != nil {
		// A DANGLING link — what an upgrade eventually leaves behind, since herdr
		// installs each release in its own directory and removes the old one
		// (at once before 0.9.5, after the herdr server exits since). Refusing
		// here (the old behaviour) left the operator's `hap` command broken
		// with no in-app way to fix it.
		return Stale, nil
	}
	if resolvedTarget == resolvedSource {
		return Current, nil
	}
	// A live binary somewhere else. Supersede it only when it is recognizably
	// a hap from a herdr plugin install (a previous version of ourselves);
	// anything else is the operator's and stays untouched.
	if ownedByHerdrPlugin(resolvedTarget) {
		return Stale, nil
	}
	return Foreign, fmt.Errorf("%s already points to a different target; remove it first", target)
}

// resolveAbs canonicalizes a path through symlinks. A failure in either step
// means "cannot be resolved".
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
// cannot claim to be ours. It is layout-agnostic on purpose: herdr 0.9.5
// moved installs two directories deeper and nothing here had to change.
func ownedByHerdrPlugin(path string) bool {
	if filepath.Base(path) != name {
		return false
	}
	return selfpath.Managed(path)
}

// pointsIntoPluginTree reports whether link's own target lies inside herdr's
// plugin install tree, whether or not it still exists. Reading the link
// rather than resolving it is the point: after an update the old install
// directory is gone, and the spelling of the dead target is the only evidence
// left that the link was hap's.
func pointsIntoPluginTree(link string) bool {
	target, err := os.Readlink(link)
	if err != nil {
		return false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(link), target)
	}
	if ownedByHerdrPlugin(filepath.Clean(target)) {
		return true
	}
	// A live chain (link → link → install) is ours by where it ends.
	resolved, err := resolveAbs(link)
	return err == nil && ownedByHerdrPlugin(resolved)
}

// Ensure points target at source. Re-running is successful when target
// already resolves to source; a stale link left by an upgrade is repointed;
// an unrelated file, or a link to someone else's live binary, is left
// untouched and reported.
func Ensure(source, target string) error {
	state, err := Classify(source, target)
	if err != nil {
		return err
	}
	resolvedSource, err := resolveAbs(source)
	if err != nil {
		return fmt.Errorf("resolve symlink source %s: %w", source, err)
	}

	switch state {
	case Current:
		return nil
	case Absent:
		// ~/.local/bin does not exist on every machine; the system directory
		// always does, so this creates nothing there.
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("create %s: %w", filepath.Dir(target), err)
		}
		if err := os.Symlink(resolvedSource, target); err != nil {
			return fmt.Errorf("create %s symlink: %w", target, cause(err))
		}
		return nil
	case Stale:
		return repointSymlink(resolvedSource, target)
	}
	// Classify always pairs Foreign with an error, which the guard above
	// returned; keep a message rather than silently succeeding.
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
		return fmt.Errorf("repoint %s: %w", target, cause(err))
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("repoint %s: %w", target, cause(err))
	}
	return nil
}

// cause strips the paths os.Symlink and os.Rename put in FRONT of the reason
// ("symlink <long install path> <target>: permission denied"). The TUI shows
// an action's error on one truncated line, and with the paths first the only
// part that mattered — why — was the part cut off. The caller already names
// the target.
func cause(err error) error {
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		return linkErr.Err
	}
	return err
}
