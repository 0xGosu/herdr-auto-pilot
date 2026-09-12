package skilldoc

// The skill bundle an ORCHESTRATOR session gets on disk, as opposed to the
// home-relative `hap skill install` above.
//
// The two are deliberately separate. `hap skill install` is an operator typing
// a command, so it writes into ~ and overwrites without asking; this one runs
// UNATTENDED from the daemon, so it writes only into the directory hap itself
// owns (<state>/orchestrator) and its destination is WORKSPACE-relative, not
// home-relative — a coding agent discovers a project skill beside its cwd. That
// is also why Targets() is not reused: its agy entry documents that ~/.agents
// is NOT read globally, only per workspace.
//
// Why on disk at all, when the daemon's brief already tells the session to run
// `hap --skill`: a skill in the cwd is re-discoverable by the agent's own skill
// loader at any time, including after an automatic context compaction has
// dropped the brief from the conversation.

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// orchestratorSkillFS is the hap-orchestrator skill TREE — a SKILL.md plus its
// reference files, which SKILL.md links by relative name, so they only work
// installed beside it.
//
// The `all:` prefix is load-bearing: a directory pattern drops children whose
// names begin with "." or "_", so a later .gitkeep or _draft.md would silently
// stop shipping. (The leading-dot ANCESTOR is fine — only wildcard walks skip
// those, and this path is named explicitly.)
//
//go:embed all:.claude/skills/hap-orchestrator
var orchestratorSkillFS embed.FS

const (
	// orchestratorSkillRoot is the embedded tree's root, in slash form (an
	// embed.FS is always slash-separated, whatever the host).
	orchestratorSkillRoot = ".claude/skills/hap-orchestrator"
	// hapSkillDirName and orchestratorSkillDirName are the per-skill directory
	// names a coding agent discovers each skill under.
	hapSkillDirName          = "hap"
	orchestratorSkillDirName = "hap-orchestrator"
)

// AgentSkillsDir is the workspace-relative skills directory an agent of the
// given herdr KIND discovers project skills in. A pure function of the kind:
// claude reads .claude/skills, and every other CLI hap could host reads the
// shared .agents/skills.
//
// Only domain.OrchestratorAgentKind is reachable today — domain.OrchestratorLaunch
// refuses any other kind — so the second branch is written and tested but
// dormant until an orchestrator may be something other than claude.
func AgentSkillsDir(kind string) string {
	if kind == domain.OrchestratorAgentKind {
		return ".claude/skills"
	}
	return ".agents/skills"
}

// InstallAgentSkills writes the bundled hap and hap-orchestrator skills under
// root for an agent of the given kind, returning the paths it actually CHANGED
// (empty when everything was already current).
//
// Idempotent by content: a file whose bytes already match the embedded copy is
// left alone, so a steady state costs reads and no writes at all. A file that
// DIFFERS is replaced — including one an operator edited. That is deliberate:
// this tree lives inside the directory hap owns and re-creates, it is the same
// bargain `hap skill install` has always made, and a skill left stale across an
// upgrade is exactly the failure this exists to prevent. An operator who wants
// a hand-written skill points full_self_prompting.orchestrator_agent_cwd at a
// directory of their own, where hap writes nothing.
//
// One file failing NEVER stops the rest: each error is recorded and the walk
// carries on, so a single unwritable path costs that one file rather than every
// file after it. Aborting was the shape to avoid — fs.WalkDir walks in lexical
// order, so the same file fails on every later pass too, and SKILL.md would be
// left linking references that were never installed. The paths actually written
// come back alongside the joined errors.
func InstallAgentSkills(root, kind string) ([]string, error) {
	base := filepath.Join(root, filepath.FromSlash(AgentSkillsDir(kind)))
	var written []string
	var errs []error
	record := func(dest string, data []byte) {
		changed, err := syncSkillFile(dest, data)
		if changed {
			written = append(written, dest)
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	record(filepath.Join(base, hapSkillDirName, SkillFileName), []byte(HapSkill))
	walkErr := fs.WalkDir(orchestratorSkillFS, orchestratorSkillRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel := strings.TrimPrefix(p, orchestratorSkillRoot+"/")
		data, err := orchestratorSkillFS.ReadFile(p)
		if err != nil {
			errs = append(errs, fmt.Errorf("read embedded %s: %w", p, err))
			return nil
		}
		record(filepath.Join(base, orchestratorSkillDirName, filepath.FromSlash(rel)), data)
		return nil
	})
	return written, errors.Join(append(errs, walkErr)...)
}

// syncSkillFile writes data to path unless it is already there byte for byte,
// reporting whether it wrote. The write itself is the package's atomic
// temp-and-rename, so a reader never sees half a skill.
func syncSkillFile(path string, data []byte) (bool, error) {
	if current, err := os.ReadFile(path); err == nil && bytes.Equal(current, data) {
		return false, nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, fmt.Errorf("create %s: %w", dir, err)
	}
	if err := writeFileAtomic(path, data); err != nil {
		return false, err
	}
	return true, nil
}
