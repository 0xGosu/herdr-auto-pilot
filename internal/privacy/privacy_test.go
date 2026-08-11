// Package privacy holds the no-telemetry verification (NFR-007, SC-6): the
// plugin sends no telemetry and makes no outbound network call beyond the
// local Herdr socket, the operator-configured local LLM CLI, and TWO
// allowlisted exceptions, both operator opt-in and both off by default:
//
//  1. internal/updatecheck/fetch.go — the opt-out release check, which asks
//     GitHub for the newest published version and sends nothing else.
//  2. internal/taskstore/gist/gist.go — the github_gist task-list backend,
//     which carries the task text of the sources configured for it, to a gist
//     the operator owns, with a token the operator supplies. No pane content,
//     no learned rules, no audit history. A source uses it only when
//     [task_source_provider] (or that source's own `provider`) selects it, and
//     the default is local_fs.
//
// Both files are allowlisted BY NAME below; net/http, or the GitHub SDK, from
// anywhere else still fails.
package privacy

import (
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// forbiddenImports are packages that would enable remote egress. "net" is
// permitted because the Herdr socket and control socket are local
// unix-domain sockets; TestNetUsageIsLocalOnly pins how it is used.
var forbiddenImports = map[string]string{
	"net/http":         "HTTP egress",
	"net/smtp":         "mail egress",
	"net/rpc":          "remote RPC",
	"crypto/tls":       "TLS connections imply remote endpoints",
	"net/url":          "", // allowed: used for DSN building only — see allowlist below
	"golang.org/x/net": "extended networking",
	// An SDK is banned by its own path, not just by the net/http it drags in.
	// The walker checks DIRECT imports of first-party files, so a future
	// adapter that used only the SDK — never naming net/http itself — would
	// egress while passing this test. Passing is not compliance.
	"github.com/google/go-github/v90/github": "GitHub REST egress",
}

// allowedNetURLFiles may import net/url for non-network purposes.
var allowedNetURLFiles = map[string]bool{
	"internal/store/store.go": true, // SQLite DSN query encoding
}

// allowedEgressFiles maps a forbidden import to the files permitted to use it.
//
// Exactly two files egress, both operator opt-in and both off by default. Every
// addition widens a promise the README makes to users, so it must be a
// deliberate edit here — TestHTTPAllowlistStaysMinimal pins the exact set.
//
// Note what is NOT here, by design rather than by luck. The gist backend uses
// github.WithURLs (which takes *string) instead of a *url.URL, so it needs no
// net/url entry; and github.WithTimeout instead of a hand-built
// http.Transport, so it constructs no net.Dialer and TestNetUsageIsLocalOnly
// stays satisfied. Keep it that way: reaching for either would widen this list
// further.
var allowedEgressFiles = map[string]map[string]string{
	"net/http": {
		"internal/updatecheck/fetch.go":   "opt-out GitHub release check (version numbers only)",
		"internal/taskstore/gist/gist.go": "opt-in github_gist task-list backend (task text only)",
	},
	"github.com/google/go-github/v90/github": {
		"internal/taskstore/gist/gist.go": "opt-in github_gist task-list backend (task text only)",
	},
}

func TestNoTelemetryImports(t *testing.T) {
	root := repoRoot(t)
	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == "testdata" || name == "docs" || name == "submodule" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if perr != nil {
			t.Errorf("parse %s: %v", rel, perr)
			return nil
		}
		for _, imp := range f.Imports {
			ip := strings.Trim(imp.Path.Value, `"`)
			reason, forbidden := forbiddenImports[ip]
			if !forbidden {
				continue
			}
			if ip == "net/url" && allowedNetURLFiles[filepath.ToSlash(rel)] {
				continue
			}
			if _, ok := allowedEgressFiles[ip][filepath.ToSlash(rel)]; ok {
				continue
			}
			if reason == "" {
				reason = "potential egress"
			}
			t.Errorf("%s imports %s (%s) — violates NFR-007 no-telemetry", rel, ip, reason)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestNetUsageIsLocalOnly pins every use of the net package to unix-domain
// transports: no "tcp"/"udp" dials anywhere in the plugin source.
func TestNetUsageIsLocalOnly(t *testing.T) {
	root := repoRoot(t)
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == "testdata" || name == "docs" || name == "submodule" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		src := string(data)
		rel, _ := filepath.Rel(root, path)
		for _, bad := range []string{`"tcp"`, `"tcp4"`, `"tcp6"`, `"udp"`, `Dial("`, `DialTimeout("`} {
			if strings.Contains(src, bad) && !strings.Contains(src, `"unix"`) {
				t.Errorf("%s contains %s without unix-domain scoping — remote egress suspected", rel, bad)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestHTTPAllowlistStaysMinimal pins the egress exceptions to an exact set.
// Widening it changes what the README promises users, so it must be a
// deliberate edit here — never a quiet addition.
//
// Two files egress, both operator opt-in and both off by default:
//   - the release check, off with [tui] disable_check_for_update;
//   - the github_gist task-list backend, used only when a task source's
//     effective provider selects it, which no config does by default.
//
// The name is unchanged so a reviewer grepping for it still lands here.
func TestHTTPAllowlistStaysMinimal(t *testing.T) {
	want := map[string]map[string]bool{
		"net/http": {
			"internal/updatecheck/fetch.go":   true,
			"internal/taskstore/gist/gist.go": true,
		},
		"github.com/google/go-github/v90/github": {
			"internal/taskstore/gist/gist.go": true,
		},
	}

	if len(allowedEgressFiles) != len(want) {
		t.Fatalf("the egress allowlist covers %d imports, want %d: %v",
			len(allowedEgressFiles), len(want), slices.Sorted(maps.Keys(allowedEgressFiles)))
	}
	root := repoRoot(t)
	for imp, wantFiles := range want {
		gotFiles, ok := allowedEgressFiles[imp]
		if !ok {
			t.Errorf("the allowlist no longer covers %q", imp)
			continue
		}
		if !slices.Equal(slices.Sorted(maps.Keys(gotFiles)), slices.Sorted(maps.Keys(wantFiles))) {
			t.Errorf("%s allowlist = %v, want %v",
				imp, slices.Sorted(maps.Keys(gotFiles)), slices.Sorted(maps.Keys(wantFiles)))
		}
		for rel, reason := range gotFiles {
			if reason == "" {
				t.Errorf("allowlist entry %s/%s has no reason", imp, rel)
			}
			if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
				t.Errorf("allowlisted file %s is missing — drop the entry instead: %v", rel, err)
			}
		}
	}
}

// TestGoGitHubImportIsAllowlistedToOneFile keeps the GitHub SDK confined to the
// single adapter file, so the networked surface stays reviewable in one place.
// Its siblings in that package (error shaping, redaction, the truncation rule)
// deliberately import nothing networked.
func TestGoGitHubImportIsAllowlistedToOneFile(t *testing.T) {
	const sdk = "github.com/google/go-github/v90/github"
	files := allowedEgressFiles[sdk]
	if len(files) != 1 {
		t.Fatalf("%s must be allowlisted for exactly one file, got %v",
			sdk, slices.Sorted(maps.Keys(files)))
	}
	if _, ok := files["internal/taskstore/gist/gist.go"]; !ok {
		t.Errorf("the SDK allowlist names %v, want the gist adapter",
			slices.Sorted(maps.Keys(files)))
	}
	if _, forbidden := forbiddenImports[sdk]; !forbidden {
		t.Error("the SDK must stay in forbiddenImports — the walker checks DIRECT imports, so " +
			"without this entry an adapter that used only the SDK would egress while passing")
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
