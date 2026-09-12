package skilldoc

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// orchestratorTreeFiles lists the embedded tree, relative to its root.
func orchestratorTreeFiles(t *testing.T) []string {
	t.Helper()
	var files []string
	err := fs.WalkDir(orchestratorSkillFS, orchestratorSkillRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		files = append(files, strings.TrimPrefix(p, orchestratorSkillRoot+"/"))
		return nil
	})
	if err != nil {
		t.Fatalf("walk the embedded tree: %v", err)
	}
	slices.Sort(files)
	return files
}

// The orchestrator skill is a TREE whose SKILL.md links its references by
// relative name, so a single-document embed would ship it broken. Compared
// against the real directory: a reference file added to the repo and not to the
// bundle is the regression this catches, and `all:` is what keeps a later
// .gitkeep or _draft.md from dropping out silently.
func TestOrchestratorSkillTreeIsEmbeddedWholly(t *testing.T) {
	embedded := orchestratorTreeFiles(t)
	if len(embedded) < 2 {
		t.Fatalf("the embedded tree holds %v — a multi-file skill was expected", embedded)
	}
	if !slices.Contains(embedded, SkillFileName) {
		t.Errorf("the embedded tree has no %s: %v", SkillFileName, embedded)
	}

	root := filepath.FromSlash(orchestratorSkillRoot)
	var onDisk []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		onDisk = append(onDisk, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	slices.Sort(onDisk)
	if !slices.Equal(embedded, onDisk) {
		t.Errorf("embedded tree = %v, the repo holds %v", embedded, onDisk)
	}

	doc, err := orchestratorSkillFS.ReadFile(orchestratorSkillRoot + "/" + SkillFileName)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc), "name: hap-orchestrator") {
		t.Errorf("the embedded document does not carry the hap-orchestrator frontmatter:\n%.200s", doc)
	}
}

// Only the claude branch is reachable today (domain.OrchestratorLaunch refuses
// any other kind); the other is written and tested for when that changes.
func TestAgentSkillsDirIsPerKind(t *testing.T) {
	if got := AgentSkillsDir(domain.OrchestratorAgentKind); got != ".claude/skills" {
		t.Errorf("AgentSkillsDir(%q) = %q, want .claude/skills", domain.OrchestratorAgentKind, got)
	}
	for _, kind := range []string{"codex", "agy", "", "something-new"} {
		if got := AgentSkillsDir(kind); got != ".agents/skills" {
			t.Errorf("AgentSkillsDir(%q) = %q, want .agents/skills", kind, got)
		}
	}
}

func TestInstallAgentSkillsWritesBothSkills(t *testing.T) {
	root := t.TempDir()
	written, err := InstallAgentSkills(root, domain.OrchestratorAgentKind)
	if err != nil {
		t.Fatalf("InstallAgentSkills: %v", err)
	}
	base := filepath.Join(root, ".claude", "skills")

	hap := filepath.Join(base, "hap", SkillFileName)
	got, err := os.ReadFile(hap)
	if err != nil {
		t.Fatalf("read %s: %v", hap, err)
	}
	if string(got) != HapSkill {
		t.Errorf("%s differs from the embedded hap skill", hap)
	}

	want := []string{hap}
	for _, rel := range orchestratorTreeFiles(t) {
		dest := filepath.Join(base, "hap-orchestrator", filepath.FromSlash(rel))
		want = append(want, dest)
		got, err := os.ReadFile(dest)
		if err != nil {
			t.Fatalf("read %s: %v", dest, err)
		}
		embedded, err := orchestratorSkillFS.ReadFile(orchestratorSkillRoot + "/" + rel)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(embedded) {
			t.Errorf("%s differs from the embedded copy", dest)
		}
	}
	if !slices.Equal(written, want) {
		t.Errorf("written = %v, want %v", written, want)
	}
}

// Every other kind lands under .agents/skills instead, same layout.
func TestInstallAgentSkillsUsesTheSharedDirForOtherKinds(t *testing.T) {
	root := t.TempDir()
	if _, err := InstallAgentSkills(root, "codex"); err != nil {
		t.Fatalf("InstallAgentSkills: %v", err)
	}
	for _, rel := range []string{
		filepath.Join("hap", SkillFileName),
		filepath.Join("hap-orchestrator", SkillFileName),
	} {
		if _, err := os.Stat(filepath.Join(root, ".agents", "skills", rel)); err != nil {
			t.Errorf("missing %s: %v", rel, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, ".claude")); !os.IsNotExist(err) {
		t.Errorf("a non-claude kind wrote .claude (stat err = %v)", err)
	}
}

// A steady state does no writes at all: the daemon runs this on every pass in
// which the orchestrator is not yet healthy.
func TestInstallAgentSkillsIsIdempotent(t *testing.T) {
	root := t.TempDir()
	first, err := InstallAgentSkills(root, domain.OrchestratorAgentKind)
	if err != nil || len(first) == 0 {
		t.Fatalf("first install wrote %v (%v)", first, err)
	}
	dest := filepath.Join(root, ".claude", "skills", "hap", SkillFileName)
	before, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}

	second, err := InstallAgentSkills(root, domain.OrchestratorAgentKind)
	if err != nil {
		t.Fatalf("second install: %v", err)
	}
	if len(second) != 0 {
		t.Errorf("second install rewrote %v, want nothing", second)
	}
	after, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("an unchanged file was rewritten (%v → %v)", before.ModTime(), after.ModTime())
	}
}

// A file that differs — an older release's copy, or one an operator edited — is
// replaced. hap owns this tree; the refusal to touch an operator's own
// directory is what protects a hand-written skill (see the daemon's
// bootstrapOrchestratorSkills).
func TestInstallAgentSkillsRefreshesAChangedFile(t *testing.T) {
	root := t.TempDir()
	if _, err := InstallAgentSkills(root, domain.OrchestratorAgentKind); err != nil {
		t.Fatal(err)
	}
	edited := filepath.Join(root, ".claude", "skills", "hap-orchestrator", SkillFileName)
	if err := os.WriteFile(edited, []byte("an older release's skill"), 0o644); err != nil {
		t.Fatal(err)
	}

	written, err := InstallAgentSkills(root, domain.OrchestratorAgentKind)
	if err != nil {
		t.Fatalf("InstallAgentSkills: %v", err)
	}
	if !slices.Equal(written, []string{edited}) {
		t.Errorf("written = %v, want only the changed %q", written, edited)
	}
	got, err := os.ReadFile(edited)
	if err != nil {
		t.Fatal(err)
	}
	embedded, err := orchestratorSkillFS.ReadFile(orchestratorSkillRoot + "/" + SkillFileName)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(embedded) {
		t.Error("the stale copy was not refreshed")
	}
}

// One unwritable file costs THAT file and nothing else. Aborting the walk was
// the shape to avoid: fs.WalkDir goes in lexical order, so the same file fails
// on every later pass too and SKILL.md would be left linking references that
// were never installed.
func TestInstallAgentSkillsInstallsTheRestAroundAFailure(t *testing.T) {
	tree := orchestratorTreeFiles(t)
	if len(tree) < 3 {
		t.Skipf("need a tree with a middle file, have %v", tree)
	}
	// A file that sorts in the middle, so both earlier and later ones prove the
	// walk neither stopped nor skipped ahead.
	blocked := tree[len(tree)/2]

	root := t.TempDir()
	// A non-empty DIRECTORY where the file must go: its parent is created fine,
	// so only this one file's rename fails.
	inTheWay := filepath.Join(root, ".claude", "skills", "hap-orchestrator", filepath.FromSlash(blocked))
	if err := os.MkdirAll(inTheWay, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inTheWay, "keep"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	written, err := InstallAgentSkills(root, domain.OrchestratorAgentKind)
	if err == nil {
		t.Fatal("expected the blocked file to fail")
	}
	if !strings.Contains(err.Error(), blocked) {
		t.Errorf("the error does not name the blocked file %q: %v", blocked, err)
	}
	for _, rel := range tree {
		dest := filepath.Join(root, ".claude", "skills", "hap-orchestrator", filepath.FromSlash(rel))
		if rel == blocked {
			if slices.Contains(written, dest) {
				t.Errorf("%s was reported written despite failing", rel)
			}
			continue
		}
		if !slices.Contains(written, dest) {
			t.Errorf("%s was not installed — the walk stopped at %s", rel, blocked)
		}
		if _, statErr := os.Stat(dest); statErr != nil {
			t.Errorf("%s missing on disk: %v", rel, statErr)
		}
	}
	// The hap document is written before the tree and is unaffected.
	if _, statErr := os.Stat(filepath.Join(root, ".claude", "skills", "hap", SkillFileName)); statErr != nil {
		t.Errorf("the hap skill was not installed: %v", statErr)
	}
}

// A failure partway still reports what DID land, so the caller can say so.
func TestInstallAgentSkillsReportsPartialWritesOnFailure(t *testing.T) {
	root := t.TempDir()
	// A plain file where the hap-orchestrator directory must go fails that
	// skill's MkdirAll after the hap document was written.
	dir := filepath.Join(root, ".claude", "skills")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hap-orchestrator"), []byte("in the way"), 0o644); err != nil {
		t.Fatal(err)
	}

	written, err := InstallAgentSkills(root, domain.OrchestratorAgentKind)
	if err == nil {
		t.Fatal("expected the hap-orchestrator write to fail")
	}
	want := filepath.Join(dir, "hap", SkillFileName)
	if !slices.Equal(written, []string{want}) {
		t.Fatalf("written = %v, want the already-installed %q", written, want)
	}
}
