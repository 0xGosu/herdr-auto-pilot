package taskstore_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// localFileMutators are taskfile's PATH-taking entry points: they os.Stat, read
// and atomically rewrite a file on this machine. taskfile's other exports are
// pure content transforms (Reserve, Release, Reclaim, ApplyReview, ExpectText)
// or lock helpers, and every caller is free to use those.
var localFileMutators = map[string]bool{
	"Mutate":          true,
	"MutateWithin":    true,
	"WriteFileAtomic": true,
}

// localFileMutatorCallers are the files allowed to call one, WITH the reason.
var localFileMutatorCallers = map[string]string{
	"internal/taskstore/local/local.go": "the local backend IS the local file, which is the one place a locator is a path",
}

// TestOnlyTheLocalBackendTouchesATaskListAsAFile keeps a task-list LOCATOR from
// being handled as a filesystem path outside the one backend where it is one.
//
// A locator is not a path. Under a remote provider it is a `gist://<id>/<file>`
// URI, and os.Stat can only ever fail on it — with `stat gist://…: no such file
// or directory`, an error that reads like a missing file rather than like code
// looking in the wrong place. That is not hypothetical: the generated-task
// confirm moved its read and write to the store and left the send-time
// RESERVATION on taskfile.Mutate, so under a `github_gist` provider it created
// the list, registered the source, CLAIMED the escalation, and only then died
// on that stat with nothing sent — while every unit test, all of them local,
// stayed green.
//
// The class is what needs closing, not the instance: any future call site that
// reaches for the local twin of a store method breaks the same way and passes
// the same tests. Adding a legitimate caller means adding it here with a
// reason, which is the review prompt this test exists to force.
func TestOnlyTheLocalBackendTouchesATaskListAsAFile(t *testing.T) {
	root := repoRoot(t)
	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "testdata", "docs", "submodule":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)

		// taskfile owns these, and tests exercise them directly on purpose.
		if strings.HasPrefix(rel, "internal/taskfile/") || strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		if _, allowed := localFileMutatorCallers[rel]; allowed {
			return nil
		}

		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Errorf("parse %s: %v", rel, perr)
			return nil
		}
		// The import alias, so a renamed import is still caught.
		alias := "taskfile"
		imported := false
		for _, imp := range f.Imports {
			if strings.Trim(imp.Path.Value, `"`) != "github.com/0xGosu/herdr-auto-pilot/internal/taskfile" {
				continue
			}
			imported = true
			if imp.Name == nil {
				continue
			}
			if imp.Name.Name == "." {
				// A dot-import makes every call a bare `Mutate(path, fn)` Ident
				// with no selector to match, so the scan below cannot see it at
				// all. Refuse the import itself rather than let the ban lapse.
				t.Errorf("%s:%d dot-imports internal/taskfile, which makes its path-taking "+
					"mutators invisible to this check — import it normally.",
					rel, fset.Position(imp.Pos()).Line)
				return nil
			}
			alias = imp.Name.Name
		}
		if !imported {
			return nil
		}

		// Matched on the SELECTOR, not on a call: `f := taskfile.Mutate` hands
		// the function to a caller this walk would never see, and there is no
		// legitimate non-call reference to any of these today.
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != alias || !localFileMutators[sel.Sel.Name] {
				return true
			}
			t.Errorf("%s:%d references taskfile.%s on a task-list locator — a locator is not a path. "+
				"Under a remote provider it is a gist:// URI that os.Stat can only fail on, so this "+
				"works in every (local) test and can never work for the operator. Go through the "+
				"store (ports.TaskStore / mutateList), or add this file to localFileMutatorCallers "+
				"with a reason.", rel, fset.Position(sel.Pos()).Line, sel.Sel.Name)
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestLocalFileMutatorExemptionsAreLive keeps that exemption list from rotting:
// an entry naming a file that no longer exists would silently cover a future
// file that reused the path.
func TestLocalFileMutatorExemptionsAreLive(t *testing.T) {
	root := repoRoot(t)
	for rel, why := range localFileMutatorCallers {
		if why == "" {
			t.Errorf("exemption %q has no reason", rel)
		}
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Errorf("exemption %q names a file that no longer exists — drop it: %v", rel, err)
		}
	}
}
