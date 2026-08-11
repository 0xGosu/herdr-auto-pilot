package taskstore_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// concreteBackends are the packages that IMPLEMENT a task-list backend, as
// opposed to the interface every caller uses.
var concreteBackends = []string{
	"github.com/0xGosu/herdr-auto-pilot/internal/taskstore/local",
	"github.com/0xGosu/herdr-auto-pilot/internal/taskstore/gist",
}

// backendImporters are the files allowed to name a concrete backend: the
// registry that selects one, and the backends' own packages.
var backendImporters = map[string]string{
	"internal/taskstore/registry.go": "the registry is what maps config to an adapter",
}

// TestOnlyTheRegistryImportsAConcreteBackend keeps task storage an ADAPTER
// boundary rather than a convention.
//
// Everything hap does to a checklist goes through ports.TaskStore, and the
// config decides which adapter is behind it. If the daemon, a front-end or the
// CLI could reach for a concrete backend, adding the next provider would mean
// hunting every such call site — and a `local.Store` used directly would
// silently bypass a configured remote provider, forking the operator's list in
// two.
//
// Adding a legitimate importer means adding it here WITH a reason, which is the
// review prompt this test exists to force.
func TestOnlyTheRegistryImportsAConcreteBackend(t *testing.T) {
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

		// A backend's own package, and any test, may name it: tests construct
		// adapters directly on purpose, and the boundary is about production
		// call sites.
		if strings.HasPrefix(rel, "internal/taskstore/local/") ||
			strings.HasPrefix(rel, "internal/taskstore/gist/") ||
			strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		if _, allowed := backendImporters[rel]; allowed {
			return nil
		}

		f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if perr != nil {
			t.Errorf("parse %s: %v", rel, perr)
			return nil
		}
		for _, imp := range f.Imports {
			ip := strings.Trim(imp.Path.Value, `"`)
			for _, backend := range concreteBackends {
				if ip == backend {
					t.Errorf("%s imports the concrete backend %s — task storage is an adapter "+
						"boundary, so callers must take ports.TaskStore and let the registry "+
						"choose. Using a backend directly would bypass a configured provider "+
						"and fork the operator's list.", rel, ip)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestBackendImporterExemptionsAreLive keeps the exemption list from rotting:
// an entry naming a file that no longer exists would silently cover a future
// file that reused the path.
func TestBackendImporterExemptionsAreLive(t *testing.T) {
	root := repoRoot(t)
	for rel, why := range backendImporters {
		if why == "" {
			t.Errorf("exemption %q has no reason", rel)
		}
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Errorf("exemption %q names a file that no longer exists — drop it: %v", rel, err)
		}
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}
