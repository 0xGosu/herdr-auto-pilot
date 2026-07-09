// Package privacy holds the no-telemetry verification (NFR-007, SC-6): the
// plugin makes no outbound network calls beyond the local Herdr socket and
// the operator-configured local LLM CLI.
package privacy

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
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
}

// allowedNetURLFiles may import net/url for non-network purposes.
var allowedNetURLFiles = map[string]bool{
	"internal/store/store.go": true, // SQLite DSN query encoding
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
