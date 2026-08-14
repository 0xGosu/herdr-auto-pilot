// Package skilldoc bundles the agent skill documents shipped with the hap
// binary, so an installed release can print or install them without a repo
// checkout.
package skilldoc

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// HapSkill is the hap agent skill document (.claude/skills/hap/SKILL.md),
// embedded at build time. The path names the file explicitly: a leading-dot
// directory is only excluded from go:embed WILDCARD walks, not from a named
// file.
//
//go:embed .claude/skills/hap/SKILL.md
var HapSkill string

// SkillFileName is the file name the skill is installed under, inside a
// per-skill "hap" directory (the layout coding agents discover skills by).
const SkillFileName = "SKILL.md"

// Target names one agent's skill directory the bundled skill can be
// installed into.
type Target struct {
	Name  string // selector used by `hap skill install` and the TUI
	Label string // display label ("Others" covers the shared ~/.agents dir)
	Dir   string // home-relative skills directory
}

// Targets lists the supported install destinations, in display order.
func Targets() []Target {
	return []Target{
		{Name: "claude", Label: "Claude", Dir: ".claude/skills"},
		{Name: "codex", Label: "Codex", Dir: ".codex/skills"},
		{Name: "agents", Label: "Others", Dir: ".agents/skills"},
	}
}

// TargetNames lists the valid selector names, in display order.
func TargetNames() []string {
	targets := Targets()
	names := make([]string, len(targets))
	for i, t := range targets {
		names[i] = t.Name
	}
	return names
}

// DestDir is the directory the skill is installed into, as shown to the
// operator, e.g. "~/.claude/skills/hap".
func (t Target) DestDir() string {
	return "~/" + t.Dir + "/hap"
}

// Install writes the bundled skill into the named targets' skill directories
// under the current user's home, returning the paths written. A write failure
// mid-list still returns the paths already written alongside the error.
func Install(names []string) ([]string, error) {
	picked, err := pickTargets(names)
	if err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home dir: %w", err)
	}
	return installTargets(home, picked)
}

// InstallTo is Install against an explicit home directory. Each selected
// target gets <home>/<target dir>/hap/SKILL.md, created or overwritten
// atomically. An unknown name fails before anything else is written.
func InstallTo(home string, names []string) ([]string, error) {
	picked, err := pickTargets(names)
	if err != nil {
		return nil, err
	}
	return installTargets(home, picked)
}

// pickTargets validates the selection before anything touches the filesystem,
// so an argument mistake is reported as one even when $HOME is unresolvable.
func pickTargets(names []string) ([]Target, error) {
	valid := strings.Join(TargetNames(), ", ")
	if len(names) == 0 {
		return nil, fmt.Errorf("no install target named; valid targets: %s", valid)
	}
	targets := Targets()
	byName := make(map[string]Target, len(targets))
	for _, t := range targets {
		byName[t.Name] = t
	}
	picked := make([]Target, 0, len(names))
	for _, name := range names {
		t, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("unknown install target %q; valid targets: %s", name, valid)
		}
		picked = append(picked, t)
	}
	return picked, nil
}

func installTargets(home string, picked []Target) ([]string, error) {
	written := make([]string, 0, len(picked))
	for _, t := range picked {
		dir := filepath.Join(home, filepath.FromSlash(t.Dir), "hap")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return written, fmt.Errorf("create %s: %w", dir, err)
		}
		dest := filepath.Join(dir, SkillFileName)
		if err := writeFileAtomic(dest, []byte(HapSkill)); err != nil {
			return written, err
		}
		written = append(written, dest)
	}
	return written, nil
}

// writeFileAtomic replaces path via a same-directory temp file and rename, so
// a reader never sees a half-written skill. Deliberately local: the repo-wide
// guard test bans new taskfile.WriteFileAtomic callers (task-list mutators
// must go through the store), and this is not a task list.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".skill-*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	tmpName := tmp.Name()
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Chmod(0o644)
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmpName, path)
	}
	if err != nil {
		if rmErr := os.Remove(tmpName); rmErr != nil && !os.IsNotExist(rmErr) {
			err = fmt.Errorf("%w (cleanup: %v)", err, rmErr)
		}
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
