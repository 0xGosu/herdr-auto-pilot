package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/shortcut"
)

func stubInstall(t *testing.T, link shortcut.Link, err error) *int {
	t.Helper()
	orig := installShortcut
	t.Cleanup(func() { installShortcut = orig })
	calls := 0
	installShortcut = func() (shortcut.Link, error) {
		calls++
		return link, err
	}
	return &calls
}

func TestLinkReportsWhatItDid(t *testing.T) {
	const path, source = "/home/op/.local/bin/hap", "/plugins/2-0/checkout/bin/hap"
	tests := []struct {
		name   string
		state  shortcut.State
		onPath bool
		want   []string
		reject string
	}{
		{"created", shortcut.Absent, true, []string{"created " + path + " -> " + source}, "PATH"},
		{"repointed", shortcut.Stale, true, []string{"repointed " + path + " -> " + source}, "PATH"},
		{"already current", shortcut.Current, true, []string{path + " already points at this binary"}, "created"},
		// Success that still leaves `hap` unfound has to say so.
		{"off PATH", shortcut.Absent, false, []string{"created " + path, "/home/op/.local/bin is not on your PATH"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stubInstall(t, shortcut.Link{Path: path, Source: source, State: tt.state, OnPath: tt.onPath}, nil)
			var out bytes.Buffer
			if err := link(context.Background(), nil, &out, nil); err != nil {
				t.Fatalf("link: %v", err)
			}
			for _, want := range tt.want {
				if !strings.Contains(out.String(), want) {
					t.Errorf("output lacks %q:\n%s", want, out.String())
				}
			}
			if tt.reject != "" && strings.Contains(out.String(), tt.reject) {
				t.Errorf("output should not mention %q:\n%s", tt.reject, out.String())
			}
		})
	}
}

// A stale link the operator cannot write is left alone and a new one made
// elsewhere; the verb succeeds and names the one command that needs root,
// instead of failing on "permission denied" as it used to.
func TestLinkNamesTheLinkItCouldNotWrite(t *testing.T) {
	stubInstall(t, shortcut.Link{
		Path: "/Users/op/.local/bin/hap", Source: "/plugins/2-0/checkout/bin/hap",
		State: shortcut.Absent, OnPath: true, Blocked: "/usr/local/bin/hap", Shadows: true,
	}, nil)
	var out bytes.Buffer
	if err := link(context.Background(), nil, &out, nil); err != nil {
		t.Fatalf("link: %v", err)
	}
	for _, want := range []string{"created /Users/op/.local/bin/hap", "sudo rm /usr/local/bin/hap", "STILL RUNS THE OLD BUILD"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}

	// The same link later on PATH (or dead) wins nothing: say so rather than
	// alarm the operator into a sudo they do not need.
	stubInstall(t, shortcut.Link{
		Path: "/Users/op/.local/bin/hap", Source: "/plugins/2-0/checkout/bin/hap",
		State: shortcut.Absent, OnPath: true, Blocked: "/usr/local/bin/hap",
	}, nil)
	out.Reset()
	if err := link(context.Background(), nil, &out, nil); err != nil {
		t.Fatalf("link: %v", err)
	}
	if got := out.String(); strings.Contains(got, "OLD BUILD") || !strings.Contains(got, "does not win on PATH") {
		t.Errorf("output for a harmless blocked link:\n%s", got)
	}
}

func TestLinkSurfacesARefusal(t *testing.T) {
	refusal := errors.New("/usr/local/bin/hap already exists and is not a symlink")
	stubInstall(t, shortcut.Link{}, refusal)
	var out bytes.Buffer
	if err := link(context.Background(), nil, &out, nil); !errors.Is(err, refusal) {
		t.Fatalf("link err = %v, want the refusal", err)
	}
	if out.Len() != 0 {
		t.Errorf("a refused link printed a result:\n%s", out.String())
	}
}

// `hap link list` (what the registry sweep tries) must not be read as a bare
// `hap link` and change the filesystem.
func TestLinkTakesNoArguments(t *testing.T) {
	calls := stubInstall(t, shortcut.Link{}, nil)
	if err := link(context.Background(), nil, &bytes.Buffer{}, []string{"list"}); err == nil {
		t.Fatal("link accepted an argument")
	}
	if *calls != 0 {
		t.Errorf("link installed %d time(s) on a usage error", *calls)
	}
}

// The un-stubbed verb inside a test binary: the registry sweep in
// hints_test.go invokes every handler, so this refusal — not an exclusion list
// — is what keeps tests off the operator's PATH.
func TestLinkRefusesInsideATestBinary(t *testing.T) {
	err := link(context.Background(), nil, &bytes.Buffer{}, nil)
	if err == nil || !strings.Contains(err.Error(), "from a test") {
		t.Fatalf("link err = %v, want the test refusal", err)
	}
}
