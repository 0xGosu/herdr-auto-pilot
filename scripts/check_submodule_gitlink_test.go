// Package scripts_test covers the shell guards in scripts/ that CI runs
// before anything expensive. They are shell because they must work on a bare
// checkout with no toolchain; they are tested from Go so `go test ./...`
// keeps them honest.
package scripts_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	gitlinkMode = "160000"
	symlinkMode = "120000"
	// Any 40-hex string that is NOT an object in the repo. That is the
	// healthy shape: a submodule's commit lives in the submodule's own
	// object store, so the superproject cannot resolve it either.
	fakeSubmoduleSHA = "1111111111111111111111111111111111111111"
	// The guard requires this exact path, so the synthetic repos must use
	// it too.
	submodulePath = "submodule/github.com/seed-hypermedia/llama-go"
)

// scriptPath resolves the guard relative to this test file's directory.
func scriptPath(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("check-submodule-gitlink.sh")
	if err != nil {
		t.Fatalf("resolve script path: %v", err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("guard script missing: %v", err)
	}
	return abs
}

// gitEnv seals the environment every git in this file runs under.
//
// cmd.Dir alone is NOT enough: an ambient GIT_DIR/GIT_WORK_TREE/GIT_INDEX_FILE
// outranks the working directory, and git exports those to every hook and to
// `git rebase --exec` / `git bisect run` children. Inherited, these tests
// would `git init`, stage a symlinked submodule, and COMMIT it into the
// developer's real repository — manufacturing the very #265 breakage the
// guard exists to prevent. The global and system config files are nulled for
// the same reason: commit.gpgsign, core.hooksPath (this repo installs a
// commit-msg hook that rejects these messages), init.templateDir and
// core.autocrlf would each break the synthetic repos for reasons unrelated
// to the guard.
func gitEnv() []string {
	stripped := []string{
		"GIT_DIR=", "GIT_WORK_TREE=", "GIT_INDEX_FILE=", "GIT_OBJECT_DIRECTORY=",
		"GIT_COMMON_DIR=", "GIT_ALTERNATE_OBJECT_DIRECTORIES=", "GIT_CEILING_DIRECTORIES=",
		"GIT_NAMESPACE=", "GIT_CONFIG=", "GIT_CONFIG_GLOBAL=", "GIT_CONFIG_SYSTEM=",
		// Highest-precedence config injection, which GIT_CONFIG_GLOBAL=/dev/null
		// does not override, plus the hook-installing template dir.
		"GIT_CONFIG_COUNT=", "GIT_CONFIG_KEY_", "GIT_CONFIG_VALUE_", "GIT_TEMPLATE_DIR=",
	}
	env := make([]string, 0, len(os.Environ())+6)
	for _, kv := range os.Environ() {
		drop := false
		for _, prefix := range stripped {
			if strings.HasPrefix(kv, prefix) {
				drop = true
				break
			}
		}
		if !drop {
			env = append(env, kv)
		}
	}
	return append(env,
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
}

// git runs a git command in dir and fails the test on error.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// newRepo creates a repository whose .gitmodules declares submodulePath.
// The path itself is left untracked so each test can stage the shape it wants.
func newRepo(t *testing.T) string {
	t.Helper()
	requireGit(t)
	dir := t.TempDir()
	initRepo(t, dir)
	writeGitmodules(t, dir)
	git(t, dir, "add", ".gitmodules")
	return dir
}

// initRepo avoids `git init -b main`, which needs git >= 2.28: an older git
// would hard-fail these tests instead of the suite skipping cleanly.
func initRepo(t *testing.T, dir string) {
	t.Helper()
	git(t, dir, "init", "-q")
	git(t, dir, "symbolic-ref", "HEAD", "refs/heads/main")
}

func writeGitmodules(t *testing.T, dir string) {
	t.Helper()
	gitmodules := "[submodule \"" + submodulePath + "\"]\n" +
		"\tpath = " + submodulePath + "\n" +
		"\turl = https://example.com/llama-go\n"
	if err := os.WriteFile(filepath.Join(dir, ".gitmodules"), []byte(gitmodules), 0o644); err != nil {
		t.Fatalf("write .gitmodules: %v", err)
	}
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
}

// stage records path in the index with an explicit mode, which is the only
// way to synthesize a gitlink without a real submodule checkout.
func stage(t *testing.T, dir, mode, sha, path string) {
	t.Helper()
	git(t, dir, "update-index", "--add", "--cacheinfo", mode+","+sha+","+path)
}

// stageSymlink stages path as a symlink pointing at target — exactly the
// shape a worktree native-build symlink takes once it is `git add`ed.
func stageSymlink(t *testing.T, dir, path, target string) {
	t.Helper()
	stage(t, dir, symlinkMode, hashObject(t, dir, target), path)
}

func hashObject(t *testing.T, dir, content string) string {
	t.Helper()
	cmd := exec.Command("git", "hash-object", "-w", "--stdin")
	cmd.Dir = dir
	cmd.Env = gitEnv()
	cmd.Stdin = strings.NewReader(content)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git hash-object: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// runGuard executes the script inside dir and returns its exit code and
// combined output.
func runGuard(t *testing.T, dir string, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{scriptPath(t)}, args...)...)
	cmd.Dir = dir
	cmd.Env = gitEnv()
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("run guard: %v\n%s", err, out)
	}
	return exit.ExitCode(), string(out)
}

func mustContain(t *testing.T, out string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("output is missing %q:\n%s", w, out)
		}
	}
}

func TestGuardAcceptsGitlink(t *testing.T) {
	dir := newRepo(t)
	stage(t, dir, gitlinkMode, fakeSubmoduleSHA, submodulePath)
	git(t, dir, "commit", "-q", "-m", "add submodule")

	code, out := runGuard(t, dir)
	if code != 0 {
		t.Fatalf("guard rejected a valid gitlink (exit %d):\n%s", code, out)
	}
}

// The regression that broke CI twice in #265: the gitlink replaced by a
// symlink into a sibling checkout.
func TestGuardRejectsSymlinkedSubmodule(t *testing.T) {
	dir := newRepo(t)
	stageSymlink(t, dir, submodulePath, "../../../../worktree-agent-no1/"+submodulePath)
	git(t, dir, "commit", "-q", "-m", "symlink the submodule")

	code, out := runGuard(t, dir)
	if code != 1 {
		t.Fatalf("guard accepted a symlinked submodule (exit %d):\n%s", code, out)
	}
	// The whole point is that the message names the problem and the fix,
	// instead of surfacing as a linker error inside setup-native.sh.
	mustContain(t, out, submodulePath, "symlink", gitlinkMode, "git submodule update --init")
}

// A staged-but-uncommitted symlink must fail too — that is the moment a
// local `make check` can still prevent the bad commit.
func TestGuardRejectsStagedSymlinkBeforeCommit(t *testing.T) {
	dir := newRepo(t)
	stage(t, dir, gitlinkMode, fakeSubmoduleSHA, submodulePath)
	git(t, dir, "commit", "-q", "-m", "add submodule")
	stageSymlink(t, dir, submodulePath, "/elsewhere/llama-go")

	code, out := runGuard(t, dir)
	if code != 1 {
		t.Fatalf("guard accepted a staged symlink (exit %d):\n%s", code, out)
	}
	mustContain(t, out, "the index", submodulePath)
}

// A plain file staged AT the submodule path is the one broken shape the
// root sweep cannot see — it is not a symlink, and it is not *inside* a
// known submodule path, it IS one. Only the direct per-path index check
// catches it.
func TestGuardRejectsRegularFileStagedAtSubmodulePath(t *testing.T) {
	dir := newRepo(t)
	stage(t, dir, gitlinkMode, fakeSubmoduleSHA, submodulePath)
	git(t, dir, "commit", "-q", "-m", "add submodule")
	git(t, dir, "rm", "-q", "--cached", "--", submodulePath)
	stage(t, dir, "100644", hashObject(t, dir, "not a submodule\n"), submodulePath)

	code, out := runGuard(t, dir)
	if code != 1 {
		t.Fatalf("guard accepted a regular file at the submodule path (exit %d):\n%s", code, out)
	}
	mustContain(t, out, "the index records it", "a regular file", submodulePath)
}

// A staged deletion of the gitlink leaves nothing for the sweep to iterate,
// so again only the per-path check sees it. This is the shape a botched
// `git rm --cached` takes, and catching it before the commit is the whole
// point of the index leg.
func TestGuardRejectsStagedDeletionOfGitlink(t *testing.T) {
	dir := newRepo(t)
	stage(t, dir, gitlinkMode, fakeSubmoduleSHA, submodulePath)
	git(t, dir, "commit", "-q", "-m", "add submodule")
	git(t, dir, "rm", "-q", "--cached", "--", submodulePath)

	code, out := runGuard(t, dir)
	if code != 1 {
		t.Fatalf("guard accepted a staged deletion of the gitlink (exit %d):\n%s", code, out)
	}
	mustContain(t, out, "the index records it", "absent", submodulePath)
}

// The mirror image, and the leg nothing else covers: the committed tree is
// broken while the index has already been repaired. Without this, deleting
// the guard's whole ref check leaves every other test in this file green.
func TestGuardRejectsBrokenTreeWithCleanIndex(t *testing.T) {
	dir := newRepo(t)
	stageSymlink(t, dir, submodulePath, "/elsewhere/llama-go")
	git(t, dir, "commit", "-q", "-m", "symlink the submodule")
	// Repair only the index; HEAD still carries the symlink.
	git(t, dir, "rm", "-q", "--cached", "--", submodulePath)
	stage(t, dir, gitlinkMode, fakeSubmoduleSHA, submodulePath)

	code, out := runGuard(t, dir)
	if code != 1 {
		t.Fatalf("guard accepted a broken HEAD with a clean index (exit %d):\n%s", code, out)
	}
	mustContain(t, out, "HEAD records it", "symlink")
	// Both legs report independently, so this would fire if the "repair"
	// above had not actually cleaned the index.
	if strings.Contains(out, "the index records it") {
		t.Errorf("the index is clean but was blamed:\n%s", out)
	}
}

// The shape a mode comparison alone cannot see, and the one the guard's own
// remediation text used to hand people: mode 160000 pointing at a blob.
// `git update-index --cacheinfo` takes any object id without checking its
// type, so re-staging the symlink's blob as a gitlink looks repaired and
// still fails `git submodule update` with "reference is not a tree".
func TestGuardRejectsGitlinkNamingANonCommit(t *testing.T) {
	dir := newRepo(t)
	blob := hashObject(t, dir, "/elsewhere/llama-go")
	stage(t, dir, gitlinkMode, blob, submodulePath)
	git(t, dir, "commit", "-q", "-m", "gitlink pointing at a blob")

	code, out := runGuard(t, dir)
	if code != 1 {
		t.Fatalf("guard accepted a gitlink naming a blob (exit %d):\n%s", code, out)
	}
	mustContain(t, out, submodulePath, "is a blob, not a commit")
}

// A submodule commit that simply is not in the superproject's object store
// is the NORMAL case (it lives in the submodule's own store), so an
// unresolvable sha must pass — otherwise the guard would fail every clone
// that has not fetched the submodule yet.
func TestGuardAcceptsGitlinkWhoseCommitIsNotFetched(t *testing.T) {
	dir := newRepo(t)
	stage(t, dir, gitlinkMode, fakeSubmoduleSHA, submodulePath)
	git(t, dir, "commit", "-q", "-m", "unfetched submodule commit")

	if code, out := runGuard(t, dir); code != 0 {
		t.Fatalf("guard rejected an unfetched submodule commit (exit %d):\n%s", code, out)
	}
}

// A .gitmodules the guard cannot parse must fail loudly. Silently skipping
// the entry would leave that submodule unguarded while the run still
// reported OK.
func TestGuardRejectsUnparseableGitmodules(t *testing.T) {
	dir := newRepo(t)
	stage(t, dir, gitlinkMode, fakeSubmoduleSHA, submodulePath)
	gitmodules := "[submodule \"" + submodulePath + "\"]\n" +
		"\tpath = " + submodulePath + "\n" +
		"\turl = https://example.com/llama-go\n" +
		"[submodule \"submodule/github.com/acme/my repo\"]\n" +
		"\tpath = submodule/github.com/acme/my repo\n" +
		"\turl = https://example.com/other\n"
	if err := os.WriteFile(filepath.Join(dir, ".gitmodules"), []byte(gitmodules), 0o644); err != nil {
		t.Fatalf("write .gitmodules: %v", err)
	}
	git(t, dir, "add", ".gitmodules")
	git(t, dir, "commit", "-q", "-m", "submodule path with a space")

	code, out := runGuard(t, dir)
	if code != 1 {
		t.Fatalf("guard accepted an unparseable .gitmodules (exit %d):\n%s", code, out)
	}
	mustContain(t, out, "whitespace")
}

// Submodule contents committed as ordinary files is the other way the
// gitlink disappears (a `git add -f` over a materialized checkout).
func TestGuardRejectsSubmoduleCommittedAsFiles(t *testing.T) {
	dir := newRepo(t)
	inner := filepath.Join(dir, submodulePath)
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(inner, "README.md"), []byte("vendored\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	git(t, dir, "add", submodulePath)
	git(t, dir, "commit", "-q", "-m", "vendor the submodule")

	code, out := runGuard(t, dir)
	if code != 1 {
		t.Fatalf("guard accepted vendored submodule files (exit %d):\n%s", code, out)
	}
	// Assert on the message, not just the exit code: exit 1 alone would
	// also be produced by the guard failing for an unrelated reason.
	mustContain(t, out, submodulePath, "expected a gitlink")
}

// A .gitmodules entry with no tree entry at all is broken the same way.
func TestGuardRejectsMissingSubmoduleEntry(t *testing.T) {
	dir := newRepo(t)
	git(t, dir, "commit", "-q", "-m", "gitmodules only")

	code, out := runGuard(t, dir)
	if code != 1 {
		t.Fatalf("guard accepted a missing submodule entry (exit %d):\n%s", code, out)
	}
	mustContain(t, out, submodulePath, "absent")
}

// The guard must not take .gitmodules as its only source of truth: a commit
// that drops the entry (a botched deinit, a merge that deleted the file)
// leaves the same unbuildable clone, so it must still fail when the gitlink
// is gone — and still pass when the gitlink is intact.
func TestGuardIsIndependentOfGitmodules(t *testing.T) {
	requireGit(t)

	t.Run("gitlink intact, no .gitmodules", func(t *testing.T) {
		dir := t.TempDir()
		initRepo(t, dir)
		stage(t, dir, gitlinkMode, fakeSubmoduleSHA, submodulePath)
		git(t, dir, "commit", "-q", "-m", "gitlink without .gitmodules")

		// An intact gitlink is not enough on its own: without a url,
		// `git submodule update --init` fatals and the clone is just as
		// unbuildable, so this must fail — for the mapping, not the mode.
		code, out := runGuard(t, dir)
		if code != 1 {
			t.Fatalf("guard passed a gitlink with no .gitmodules mapping (exit %d):\n%s", code, out)
		}
		mustContain(t, out, "no .gitmodules mapping", submodulePath)
	})

	t.Run("gitlink gone, no .gitmodules", func(t *testing.T) {
		dir := t.TempDir()
		initRepo(t, dir)
		if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("x\n"), 0o644); err != nil {
			t.Fatalf("write file: %v", err)
		}
		git(t, dir, "add", "README.md")
		git(t, dir, "commit", "-q", "-m", "no submodule at all")

		code, out := runGuard(t, dir)
		if code != 1 {
			t.Fatalf("guard passed with the required submodule gone (exit %d):\n%s", code, out)
		}
		mustContain(t, out, submodulePath, "absent")
	})
}

// A symlink under the submodule root that no .gitmodules declares is the
// same breakage wearing a different name; the sweep must catch it.
func TestGuardRejectsUndeclaredSymlinkUnderSubmoduleRoot(t *testing.T) {
	dir := newRepo(t)
	stage(t, dir, gitlinkMode, fakeSubmoduleSHA, submodulePath)
	stageSymlink(t, dir, "submodule/github.com/example/other", "/elsewhere/other")
	git(t, dir, "commit", "-q", "-m", "undeclared symlink")

	code, out := runGuard(t, dir)
	if code != 1 {
		t.Fatalf("guard accepted an undeclared symlink (exit %d):\n%s", code, out)
	}
	mustContain(t, out, "submodule/github.com/example/other", "symlink")
}

// Ordinary tracked files that live under the submodule root but outside any
// submodule (this repo's submodule/github.com/.gitkeep) are legitimate and
// must not trip the sweep.
func TestGuardAllowsPlainFilesUnderSubmoduleRoot(t *testing.T) {
	dir := newRepo(t)
	stage(t, dir, gitlinkMode, fakeSubmoduleSHA, submodulePath)
	keep := filepath.Join(dir, "submodule", "github.com")
	if err := os.MkdirAll(keep, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(keep, ".gitkeep"), nil, 0o644); err != nil {
		t.Fatalf("write .gitkeep: %v", err)
	}
	git(t, dir, "add", "submodule/github.com/.gitkeep")
	git(t, dir, "commit", "-q", "-m", "placeholder")

	if code, out := runGuard(t, dir); code != 0 {
		t.Fatalf("guard rejected a legitimate placeholder file (exit %d):\n%s", code, out)
	}
}

// An explicit ref argument must be honoured, and the index must still be
// checked alongside it.
func TestGuardChecksTheGivenRef(t *testing.T) {
	dir := newRepo(t)
	stage(t, dir, gitlinkMode, fakeSubmoduleSHA, submodulePath)
	git(t, dir, "commit", "-q", "-m", "good")
	git(t, dir, "branch", "good")
	stageSymlink(t, dir, submodulePath, "/elsewhere/llama-go")
	git(t, dir, "commit", "-q", "-m", "bad")

	// HEAD is broken.
	code, out := runGuard(t, dir, "HEAD")
	if code != 1 {
		t.Fatalf("guard accepted broken HEAD (exit %d):\n%s", code, out)
	}
	mustContain(t, out, "HEAD records it", submodulePath)

	// The good branch's tree is fine, but the index still carries the
	// symlink, so the guard must still fail — and blame the index.
	code, out = runGuard(t, dir, "good")
	if code != 1 {
		t.Fatalf("guard ignored the broken index (exit %d):\n%s", code, out)
	}
	mustContain(t, out, "the index records it")
	if strings.Contains(out, "good records it") {
		t.Errorf("ref 'good' was reported broken but its tree is fine:\n%s", out)
	}
}

// The repository this test runs in must itself be clean — the guard is only
// useful if it is true here, and this is the assertion that fails the PR
// that reintroduces the symlink.
func TestThisRepositoryHasIntactGitlinks(t *testing.T) {
	requireGit(t)
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	code, out := runGuard(t, root)
	// Exit 2 is "not a git repository at all" — an unpacked source archive
	// or a checkout git refuses over dubious ownership. Nothing to assert
	// there, and accusing it of a broken gitlink would be wrong.
	if code == 2 {
		t.Skipf("source tree is not a usable git checkout:\n%s", out)
	}
	if code != 0 {
		t.Fatalf("submodule gitlinks are broken in this checkout (exit %d):\n%s", code, out)
	}
}
