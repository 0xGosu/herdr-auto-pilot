package tasklocator_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/tasklocator"
)

// TestCanonicalLeavesAGistLocatorVerbatim is the guard against the silent
// corruption this package exists to prevent. filepath.Abs does not FAIL on a
// gist locator — it returns "<cwd>/gist:/id/f.md", differently in every process
// — so the negative case is spelled out explicitly.
func TestCanonicalLeavesAGistLocatorVerbatim(t *testing.T) {
	const locator = "gist://3f2a1b9c/brave-otter.md"

	got := tasklocator.Canonical(locator)
	if got != locator {
		t.Errorf("Canonical(%q) = %q, want it returned verbatim", locator, got)
	}

	// The exact string the un-branched implementation would have produced.
	// If this ever equals got, the scheme branch was lost.
	mangled, err := filepath.Abs(config.ExpandPath(locator))
	if err != nil {
		t.Fatal(err)
	}
	if got == mangled {
		t.Errorf("Canonical returned the cwd-relative form %q — every hap process would key "+
			"this source differently", mangled)
	}
	if !strings.Contains(mangled, "gist:/") {
		t.Fatalf("sanity: expected Abs to mangle the scheme, got %q", mangled)
	}

	// Canonical must be idempotent, since callers apply it defensively.
	if again := tasklocator.Canonical(got); again != got {
		t.Errorf("Canonical is not idempotent: %q -> %q", got, again)
	}
}

// TestCanonicalMatchesTheOldPathBehaviorExactly pins that nothing changed for a
// filesystem locator — the default provider must be byte-for-byte as before.
func TestCanonicalMatchesTheOldPathBehaviorExactly(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "tasks.md")
	if err := os.WriteFile(real, []byte("- [ ] a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.md")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	t.Setenv("HAPTEST_TASKDIR", dir)

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"absolute path", real, real},
		{"env var", "$HAPTEST_TASKDIR/tasks.md", real},
		{"braced env var", "${HAPTEST_TASKDIR}/tasks.md", real},
		{"symlink resolves to its target", link, real},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tasklocator.Canonical(tc.in)
			// Compare through EvalSymlinks rather than by string equality:
			// macOS temp dirs live under the /var -> /private/var symlink.
			wantResolved, err := filepath.EvalSymlinks(tc.want)
			if err != nil {
				t.Fatal(err)
			}
			if got != wantResolved {
				t.Errorf("Canonical(%q) = %q, want %q", tc.in, got, wantResolved)
			}
		})
	}

	t.Run("relative path becomes absolute", func(t *testing.T) {
		got := tasklocator.Canonical("tasks.md")
		if !filepath.IsAbs(got) {
			t.Errorf("Canonical(%q) = %q, want an absolute path", "tasks.md", got)
		}
	})

	t.Run("home shorthand expands", func(t *testing.T) {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Skip("no home dir")
		}
		got := tasklocator.Canonical("~/tasks.md")
		if strings.HasPrefix(got, "~") {
			t.Errorf("Canonical(%q) = %q, want ~ expanded", "~/tasks.md", got)
		}
		if !strings.HasPrefix(got, filepath.Clean(home)) {
			// EvalSymlinks may rewrite the home prefix; only assert it left ~.
			t.Logf("home %q did not prefix %q (symlinked home is fine)", home, got)
		}
	})
}

func TestSchemeAndParseGist(t *testing.T) {
	cases := []struct {
		name       string
		locator    string
		wantScheme string
		wantID     string
		wantFile   string
		wantOK     bool
	}{
		{"gist locator", "gist://abc123/brave-otter.md", tasklocator.GistScheme, "abc123", "brave-otter.md", true},
		{"absolute path", "/home/me/tasks.md", "", "", "", false},
		{"relative path", "tasks.md", "", "", "", false},
		{"empty", "", "", "", "", false},
		{"scheme with no file", "gist://abc123", tasklocator.GistScheme, "", "", false},
		{"scheme with empty file", "gist://abc123/", tasklocator.GistScheme, "", "", false},
		{"scheme with no id", "gist:///tasks.md", tasklocator.GistScheme, "", "", false},
		// A nested name means the locator was built from something path-shaped;
		// flattening it silently would point at the wrong file.
		{"scheme with a nested file", "gist://abc123/docs/tasks.md", tasklocator.GistScheme, "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tasklocator.Scheme(tc.locator); got != tc.wantScheme {
				t.Errorf("Scheme(%q) = %q, want %q", tc.locator, got, tc.wantScheme)
			}
			if got := tasklocator.Remote(tc.locator); got != (tc.wantScheme != "") {
				t.Errorf("Remote(%q) = %v, want %v", tc.locator, got, tc.wantScheme != "")
			}
			ref, ok := tasklocator.ParseGist(tc.locator)
			if ok != tc.wantOK {
				t.Fatalf("ParseGist(%q) ok = %v, want %v", tc.locator, ok, tc.wantOK)
			}
			if ok && (ref.GistID != tc.wantID || ref.File != tc.wantFile) {
				t.Errorf("ParseGist(%q) = %+v, want id=%q file=%q", tc.locator, ref, tc.wantID, tc.wantFile)
			}
		})
	}
}

func TestGistLocatorRoundTripsThroughParse(t *testing.T) {
	loc := tasklocator.GistLocator("3f2a1b9c", "brave-otter.md")
	ref, ok := tasklocator.ParseGist(loc)
	if !ok {
		t.Fatalf("ParseGist could not read back %q", loc)
	}
	if ref.GistID != "3f2a1b9c" || ref.File != "brave-otter.md" {
		t.Errorf("round trip lost data: %+v", ref)
	}
}

func remoteCfg(gistID string) config.Config {
	return config.Config{TaskSourceProvider: config.TaskSourceProvider{
		Provider:   config.ProviderGitHubGist,
		EnvFile:    "/etc/hap/task.env",
		GitHubGist: config.GitHubGist{GistID: gistID},
	}}
}

func TestResolveAppliesProviderDefaultsAndOverrides(t *testing.T) {
	local := filepath.Join(t.TempDir(), "tasks.md")

	cases := []struct {
		name    string
		cfg     config.Config
		src     config.TaskSource
		agent   string
		want    string
		wantErr string
	}{
		{
			name: "local source keeps its canonical path",
			cfg:  config.Default(), src: config.TaskSource{Path: local},
			agent: "brave-otter", want: tasklocator.Canonical(local),
		},
		{
			name: "local source ignores the agent name",
			cfg:  config.Default(), src: config.TaskSource{Path: local},
			agent: "", want: tasklocator.Canonical(local),
		},
		{
			name: "gist source with an explicit shared file",
			cfg:  remoteCfg("3f2a"), src: config.TaskSource{Path: "shared-backlog.md"},
			agent: "brave-otter", want: "gist://3f2a/shared-backlog.md",
		},
		{
			name: "gist source derives the file from the matched agent",
			cfg:  remoteCfg("3f2a"), src: config.TaskSource{},
			agent: "brave-otter", want: "gist://3f2a/brave-otter.md",
		},
		{
			// The whole point of the catch-all form: one entry, one list per agent.
			name: "a catch-all gist source derives per agent",
			cfg:  remoteCfg("3f2a"), src: config.TaskSource{Agent: ""},
			agent: "calm-badger", want: "gist://3f2a/calm-badger.md",
		},
		{
			name: "a source's own gist id outranks the default",
			cfg:  remoteCfg("3f2a"), src: config.TaskSource{GistID: "aa11"},
			agent: "brave-otter", want: "gist://aa11/brave-otter.md",
		},
		{
			name: "a source overridden back to local uses its path",
			cfg:  remoteCfg("3f2a"), src: config.TaskSource{Provider: config.ProviderLocalFS, Path: local},
			agent: "brave-otter", want: tasklocator.Canonical(local),
		},
		{
			name: "a derived source with no agent name is refused",
			cfg:  remoteCfg("3f2a"), src: config.TaskSource{},
			agent: "", wantErr: "agent name",
		},
		{
			name: "a gist source with no gist id is refused",
			cfg:  remoteCfg(""), src: config.TaskSource{Path: "a.md"},
			agent: "brave-otter", wantErr: "gist_id",
		},
		{
			name: "a local source with no path is refused",
			cfg:  config.Default(), src: config.TaskSource{},
			agent: "brave-otter", wantErr: "no path",
		},
		{
			name:  "an unknown provider is refused",
			cfg:   config.Config{TaskSourceProvider: config.TaskSourceProvider{Provider: "linear"}},
			src:   config.TaskSource{Path: "a.md"},
			agent: "brave-otter", wantErr: "unknown task source provider",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tasklocator.Resolve(tc.cfg, tc.src, tc.agent)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("want error mentioning %q, got locator %q", tc.wantErr, got.Locator)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q must mention %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Locator != tc.want {
				t.Errorf("Locator = %q, want %q", got.Locator, tc.want)
			}
		})
	}
}

// TestResolveIsAPureFunctionOfConfigAndAgent pins the property that makes a
// locator safe to persist and to compare across processes.
func TestResolveIsAPureFunctionOfConfigAndAgent(t *testing.T) {
	cfg := remoteCfg("3f2a")
	src := config.TaskSource{}

	first, err := tasklocator.Resolve(cfg, src, "brave-otter")
	if err != nil {
		t.Fatal(err)
	}
	// Change the working directory — a path-shaped implementation would move.
	// Restored on the way out: the cwd is process-global, so leaking it would
	// silently change what every later test's relative path resolves to.
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) })
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Skipf("cannot chdir: %v", err)
	}
	second, err := tasklocator.Resolve(cfg, src, "brave-otter")
	if err != nil {
		t.Fatal(err)
	}
	if first.Locator != second.Locator {
		t.Errorf("the locator moved with the working directory: %q then %q — every hap process "+
			"would key this source differently", first.Locator, second.Locator)
	}
}

func TestResolveDerivedNameIsSanitizedAndValidated(t *testing.T) {
	cfg := remoteCfg("3f2a")
	cases := []struct {
		name  string
		agent string
		want  string
	}{
		{"plain name", "brave-otter", "gist://3f2a/brave-otter.md"},
		{"name with a separator", "team/otter", "gist://3f2a/team-otter.md"},
		{"name with whitespace", "brave otter", "gist://3f2a/brave-otter.md"},
		{"name that sanitizes to nothing", "///", "gist://3f2a/agent.md"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tasklocator.Resolve(cfg, config.TaskSource{}, tc.agent)
			if err != nil {
				t.Fatalf("a sanitizable name must resolve, got %v", err)
			}
			if got.Locator != tc.want {
				t.Errorf("Locator = %q, want %q", got.Locator, tc.want)
			}
			// Whatever came out must be a legal store file name.
			ref, ok := tasklocator.ParseGist(got.Locator)
			if !ok {
				t.Fatalf("derived locator does not parse: %q", got.Locator)
			}
			if err := config.ValidateStoreFileName(ref.File); err != nil {
				t.Errorf("derived file name %q is not valid: %v", ref.File, err)
			}
		})
	}
}

func TestResolveReportsAgentNameRequiredSentinel(t *testing.T) {
	_, err := tasklocator.Resolve(remoteCfg("3f2a"), config.TaskSource{}, "")
	if !errors.Is(err, tasklocator.ErrAgentNameRequired) {
		t.Errorf("source-enumerating surfaces match on the sentinel to render a template "+
			"instead of failing; got %v", err)
	}
}

// TestDerivedFileNameMatchesTheBootstrapName pins that a gist-backed source and
// the generated-task bootstrap land on the SAME file name for one agent.
func TestDerivedFileNameMatchesTheBootstrapName(t *testing.T) {
	for _, agent := range []string{"brave-otter", "team/otter", "brave otter", "///"} {
		want := config.SanitizeTaskFileName(agent) + ".md"
		if got := tasklocator.DerivedFileName(agent); got != want {
			t.Errorf("DerivedFileName(%q) = %q, want %q — the bootstrap builds the latter",
				agent, got, want)
		}
	}
}

func TestCanonicalIsStableAcrossSpellingsOfOnePath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("path spellings differ on Windows")
	}
	dir := t.TempDir()
	real := filepath.Join(dir, "tasks.md")
	if err := os.WriteFile(real, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HAPTEST_DIR", dir)

	want := tasklocator.Canonical(real)
	for _, spelling := range []string{
		real,
		filepath.Join(dir, ".", "tasks.md"),
		filepath.Join(dir, "sub", "..", "tasks.md"),
		"$HAPTEST_DIR/tasks.md",
	} {
		if got := tasklocator.Canonical(spelling); got != want {
			t.Errorf("Canonical(%q) = %q, want %q — two spellings of one file must share "+
				"a lock and a claim key", spelling, got, want)
		}
	}
}
