package domain

import (
	"path/filepath"
	"strings"
)

// agyOutsideWorkspaceReason is the Reason text agy prints when a file lies
// outside the directory it was started in. Matched case-insensitively on a
// prefix: the reason line is agy's own wording and other reasons exist
// ("outside workspace and not under /tmp" was seen), so an exact compare
// would silently stop matching on a wording change.
const agyOutsideWorkspaceReason = "outside workspace"

// AgyOutsideWorkspacePath reports the file an agy approval is asking about
// when the reason it is asking is that the file lies outside its workspace.
//
// agy scopes permissions to the directory it was STARTED in, so an agent
// pointed at work elsewhere raises one of these per file it touches. The path
// is the evidence of where the work actually is, which is the half hap cannot
// otherwise know: the agent's own cwd never moves.
//
// Returns false for every other approval, including a file prompt with a
// different reason — the caller must not read "some approval" as this one.
func AgyOutsideWorkspacePath(pane string) (string, bool) {
	form, ok := ParseAgyApproval(pane)
	if !ok {
		return "", false
	}
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(form.Reason)),
		agyOutsideWorkspaceReason) {
		return "", false
	}
	// Target is "<Op>: <path>" ("Read: /etc/hostname"); the create prompt
	// shows a diff instead and carries none.
	_, path, found := strings.Cut(form.Target, ":")
	if !found {
		return "", false
	}
	path = strings.TrimSpace(path)
	if !strings.HasPrefix(path, "/") {
		// Only an absolute path can be compared against a workspace root.
		return "", false
	}
	return path, true
}

// AgyWorkspaceMismatch reports that path lies outside root, i.e. that this
// agent is doing its work somewhere other than where it was started.
//
// Both blanks answer FALSE, and that is the whole safety of this: root is
// read from a CACHE that is empty for a cold pane (daemon.paneCwd), so
// "we have not read it yet" must never render as "this agent is misconfigured".
// Unknown is not evidence.
//
// herdr renders a deleted directory as "/path (deleted)", so that suffix is
// stripped before comparing — otherwise every agent whose directory was
// removed would read as a mismatch against itself.
func AgyWorkspaceMismatch(root, path string) bool {
	root = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(root), "(deleted)"))
	path = strings.TrimSpace(path)
	if root == "" || path == "" || !strings.HasPrefix(root, "/") {
		return false
	}
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if root == "/" {
		// An agent started at the filesystem root contains everything; there
		// is no mismatch to report and saying so would be noise on every file.
		return false
	}
	if path == root {
		return false
	}
	// Compare on a SEPARATOR boundary: a plain prefix test makes /work/one
	// contain /work/one-other, which is a different directory entirely.
	return !strings.HasPrefix(path, root+string(filepath.Separator))
}

// AgyWorkspaceRootOf reduces a mismatching path to the root worth REPORTING —
// the first two segments ("/workspaces/worktree-agent-no23"), which is the
// shape of the directory an operator would have started the agent in.
//
// Reporting the whole file path would name whichever file happened to be read
// first, which changes on every run and tells the operator nothing about where
// to restart the agent. A shorter path is returned as-is.
func AgyWorkspaceRootOf(path string) string {
	path = filepath.Clean(strings.TrimSpace(path))
	if !strings.HasPrefix(path, "/") {
		return path
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), string(filepath.Separator))
	if len(parts) <= 2 {
		return path
	}
	return "/" + filepath.Join(parts[0], parts[1])
}
