package frontend_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// frontendTrees are the packages held to the rule. They are the OPERATOR
// surfaces — the TUI, the one-shot CLI, and the shared view/command layer both
// run on — as opposed to internal/daemon, which is the one process that may
// drive herdr.
var frontendTrees = []string{
	"internal/frontend/",
	"internal/tui/",
	"internal/cli/",
}

// herdrPorts are the ports whose implementations shell out to herdr. Reaching
// one from a front end means that process is driving a live pane itself.
//
// ports.HerdrPort is the base surface; everything else is an optional
// capability a caller type-asserts for. NotifyShower is deliberately ABSENT:
// a desktop toast never touches an agent pane and has a terminal-bell
// fallback, so it is a stated exception to this rule rather than a gap in it.
var herdrPorts = map[string]bool{
	// Herdr is App's own ports.HerdrPort field. Banning only the ports
	// SYMBOLS bans names, not the capability: `app.Herdr.Send(ctx, pane, text)`
	// names no ports symbol, imports no adapter and contains no "herdr"
	// literal, yet Send/ReadPane/ListAgents are exactly the pane-driving calls
	// this rule exists to stop — one line away, in a file with no exemption.
	// Same for a locally declared `interface{ FocusPane(...) }` asserted
	// against the field, which defeats the type-assertion scan the same way.
	"Herdr":                   true,
	"HerdrPort":               true,
	"AgentAwareSender":        true,
	"SubmitRetryWaiter":       true,
	"LocatorPort":             true,
	"InspectorPort":           true,
	"VisiblePaneReader":       true,
	"KeystrokeSender":         true,
	"KeystrokeSequenceSender": true,
	"ChordSender":             true,
	"FocusPort":               true,
	"SendToAgent":             true,
}

// herdrPortExemptions lists what each file may still reference, PER SYMBOL,
// with the reason and the migration stage that removes it.
//
// Per symbol rather than per file, because a whole-file exemption does not
// enforce the migration it records: with frontend.go blanket-exempt for the
// stages still to come, re-adding the FocusPort assertion stage 1 just deleted
// would pass green, and nothing else in the tree pins its absence. Listing the
// symbols makes each stage's removal load-bearing, and makes "the list only
// ever shrinks" a property of the test rather than of review discipline.
var herdrPortExemptions = map[string]map[string]string{
	"internal/frontend/frontend.go": {
		"Herdr":         "stages 3-5: every reader below reaches the adapter through the field",
		"HerdrPort":     "stage 6: the field's declared type, once every reader below is gone",
		"ListAgents":    "stage 3: the roster snapshot replaces the live listing",
		"InspectorPort": "stage 3: agent cwd moves to the roster",
		"LocatorPort":   "stage 3: workspace and tab labels move to the roster",
		"SendToAgent":   "stage 5: send_task carries the hand-out and the generated-task confirm",
	},
	"internal/frontend/agentmode.go": {
		"Herdr":             "stages 3-4: FindLiveAgent lists agents and the mode read/press use the field",
		"VisiblePaneReader": "stage 4: read_mode writes the mode to the roster",
		"ChordSender":       "stage 4: set_mode moves the Shift+Tab rotation to the daemon",
	},
}

// herdrBinaryExemptions lists files that may shell out to the herdr BINARY.
//
// Unlike the port list these are PERMANENT: both are plugin management, not
// agent control. Neither reads or writes an agent's pane, so neither is
// something the daemon should mediate — and selfpath in particular must keep
// working when the daemon cannot start at all.
var herdrBinaryExemptions = map[string]string{
	"internal/cli/update.go": "`hap update` manages the plugin install itself; it never touches an agent pane",
}

// TestFrontendNeverTouchesHerdr keeps the operator surfaces off herdr.
//
// The target shape is TUI/CLI -> SQLite -> daemon -> herdr. Two processes
// driving the same panes is what lets an operator's hand-out race the daemon's
// own, and none of the daemon's delivery guards — the never-auto screen, the
// kill switch, the per-agent disable, WithAgentAutomation, auto-accept's
// pre-send pane re-read — apply on a front-end path. An operator action that
// reaches a pane is therefore QUEUED (internal/store's agent_actions) and
// executed by the daemon, which is the only process holding those guards.
//
// The class is what needs closing, not the instance. Each migration stage
// deletes its own rows from herdrPortExemptions, so this test both enforces
// what has already moved and records exactly what has not.
func TestFrontendNeverTouchesHerdr(t *testing.T) {
	root := repoRoot(t)
	fset := token.NewFileSet()

	for _, tree := range frontendTrees {
		dir := filepath.Join(root, filepath.FromSlash(tree))
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			// Tests wire herdr FAKES on purpose: that is how the behaviour
			// under migration is exercised at all.
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)

			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				t.Errorf("parse %s: %v", rel, perr)
				return nil
			}

			checkHerdrAdapterImport(t, fset, f, rel)
			checkHerdrPortUse(t, fset, f, rel)
			if _, exempt := herdrBinaryExemptions[rel]; !exempt {
				checkHerdrBinaryUse(t, fset, f, rel)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", tree, err)
		}
	}
}

// checkHerdrAdapterImport refuses the adapter package outright. It is never
// exempt: importing internal/herdr from a front end is constructing the CLI
// shell-out adapter, which only cmd/hap (for the daemon) may do.
func checkHerdrAdapterImport(t *testing.T, fset *token.FileSet, f *ast.File, rel string) {
	t.Helper()
	for _, imp := range f.Imports {
		if strings.Trim(imp.Path.Value, `"`) == "github.com/0xGosu/herdr-auto-pilot/internal/herdr" {
			t.Errorf("%s:%d imports internal/herdr — the herdr adapter belongs to the daemon. "+
				"Queue an agent_actions row and let the daemon execute it.",
				rel, fset.Position(imp.Pos()).Line)
		}
	}
}

// checkHerdrPortUse scans for a herdr-backed capability.
//
// Two shapes are matched, and both are matched on the SELECTOR rather than on
// a call: `f := ports.SendToAgent` hands the function to a caller this walk
// would never see, and `h := a.Herdr` does the same for the adapter.
//
//   - `ports.X` for a herdr-backed X — the type assertions and the helper.
//   - `.Herdr` on any receiver — App's own adapter field, which reaches the
//     base surface without naming a ports symbol at all.
func checkHerdrPortUse(t *testing.T, fset *token.FileSet, f *ast.File, rel string) {
	t.Helper()
	allowed := herdrPortExemptions[rel]
	// The ports alias may be absent (a file may only touch .Herdr), which is
	// not a reason to skip the walk.
	alias, _ := importAlias(t, fset, f, rel, "github.com/0xGosu/herdr-auto-pilot/internal/ports", "ports")

	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		name := sel.Sel.Name
		if !herdrPorts[name] {
			return true
		}
		// `ports.X` is only a hit when X is reached through the ports package;
		// an unrelated `foo.LocatorPort` is not this rule's business.
		if name != "Herdr" {
			pkg, isIdent := sel.X.(*ast.Ident)
			if alias == "" || !isIdent || pkg.Name != alias {
				return true
			}
		}
		if _, ok := allowed[name]; ok {
			return true
		}
		t.Errorf("%s:%d references %s — a front-end process must not drive herdr. "+
			"None of the daemon's delivery guards apply here, and two processes typing into "+
			"one pane is the race this rule exists to prevent. Queue a domain.AgentAction and "+
			"await it (see App.Resolve), read daemon-published state from the store, or add "+
			"%q to this file's entry in herdrPortExemptions with a reason and a migration stage.",
			rel, fset.Position(sel.Pos()).Line, name, name)
		return true
	})
}

// checkHerdrBinaryUse catches a shell-out that reaches the herdr binary
// without going through the ports layer at all — the hole the port scan alone
// would leave wide open.
func checkHerdrBinaryUse(t *testing.T, fset *token.FileSet, f *ast.File, rel string) {
	t.Helper()
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		switch strings.Trim(lit.Value, "`\"") {
		case "HERDR_BIN_PATH", "herdr":
		default:
			return true
		}
		t.Errorf("%s:%d names the herdr binary — a front-end process must not shell out to "+
			"herdr. Queue a domain.AgentAction for the daemon, or add this file to "+
			"herdrBinaryExemptions with a reason.", rel, fset.Position(lit.Pos()).Line)
		return true
	})
}

// importAlias resolves the local name of an import, and refuses a dot-import.
//
// A dot-import makes every use a bare Ident with no selector to match, so the
// selector scan cannot see it at all. Refusing the import is the only way to
// keep the ban enforceable.
func importAlias(t *testing.T, fset *token.FileSet, f *ast.File, rel, path, def string) (string, bool) {
	t.Helper()
	for _, imp := range f.Imports {
		if strings.Trim(imp.Path.Value, `"`) != path {
			continue
		}
		if imp.Name == nil {
			return def, true
		}
		if imp.Name.Name == "." {
			t.Errorf("%s:%d dot-imports %s, which makes its symbols invisible to this check — "+
				"import it normally.", rel, fset.Position(imp.Pos()).Line, path)
			return "", false
		}
		return imp.Name.Name, true
	}
	return "", false
}

// TestHerdrExemptionsAreLive keeps both lists from rotting, in BOTH
// directions.
//
// An entry naming a file that no longer exists would silently cover a future
// file that reused the path — which is exactly how a migrated call site could
// come back. And a SYMBOL entry the file no longer references is the same
// hazard one level down: it is the permission a completed stage was supposed
// to hand back, so leaving it is what would let stage 1's own removal be
// undone without any test noticing. Failing on a stale entry is what makes
// "the list only ever shrinks" a property of the test.
func TestHerdrExemptionsAreLive(t *testing.T) {
	root := repoRoot(t)
	fset := token.NewFileSet()

	for rel, symbols := range herdrPortExemptions {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if _, err := os.Stat(path); err != nil {
			t.Errorf("exemption %q names a file that no longer exists — drop it: %v", rel, err)
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Errorf("parse %s: %v", rel, err)
			continue
		}
		used := map[string]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				used[sel.Sel.Name] = true
			}
			return true
		})
		for name, why := range symbols {
			if why == "" {
				t.Errorf("exemption %q/%q has no reason", rel, name)
			}
			if !used[name] {
				t.Errorf("%s no longer references %s, so its exemption is stale — delete it, "+
					"or the next reference slips back in unnoticed", rel, name)
			}
		}
	}

	for rel, why := range herdrBinaryExemptions {
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
