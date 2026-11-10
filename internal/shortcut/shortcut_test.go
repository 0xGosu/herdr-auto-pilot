package shortcut

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sandbox gives a test its own HOME, PATH, herdr config home and "system" bin
// directory, and opens the mutating entry points that refuse to run in a test
// binary otherwise. Nothing here may reach the operator's real PATH: these
// functions rewrite whatever `hap` link they find.
type sandbox struct {
	root      string
	pathDir   string // the one directory on PATH
	systemDir string // stands in for /usr/local/bin; NOT on PATH unless a test adds it
	userDir   string // ~/.local/bin; not created
	plugins   string // herdr's plugin install root
}

func newSandbox(t *testing.T) sandbox {
	t.Helper()
	// macOS temp dirs sit behind a /var → /private/var symlink; resolve once so
	// expected paths compare equal to what EvalSymlinks returns.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sb := sandbox{
		root:      root,
		pathDir:   filepath.Join(root, "path-bin"),
		systemDir: filepath.Join(root, "system-bin"),
		userDir:   filepath.Join(root, "home", ".local", "bin"),
		plugins:   filepath.Join(root, "config", "herdr", "plugins"),
	}
	for _, dir := range []string{sb.pathDir, sb.systemDir, filepath.Join(root, "home")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("PATH", sb.pathDir)
	origSystem, origAllow := systemBinDir, allowUnderTest
	systemBinDir, allowUnderTest = sb.systemDir, true
	t.Cleanup(func() { systemBinDir, allowUnderTest = origSystem, origAllow })
	return sb
}

// install095 names a binary in herdr ≥0.9.5's layout: every install or update
// gets its own <generation> directory under the plugin's install dir.
func (sb sandbox) install095(generation string) string {
	return filepath.Join(sb.plugins, "github-installations", "herd-auto-prompter-e001d0f4b256", generation, "checkout", "bin", "hap")
}

func writeExe(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func symlink(t *testing.T, dest, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dest, path); err != nil {
		t.Fatal(err)
	}
	return path
}

func resolvesTo(t *testing.T, link, want string) {
	t.Helper()
	got, err := filepath.EvalSymlinks(link)
	if err != nil {
		t.Fatalf("%s does not resolve: %v", link, err)
	}
	if got != want {
		t.Fatalf("%s resolves to %q, want %q", link, got, want)
	}
}

// TestInstallRepointsALinkLeftByAHerdr095Update is the reported breakage,
// end to end: the operator's link lives in ~/.local/bin (herdr's own install
// directory for a per-user install, and not /usr/local/bin), herdr 0.9.5
// replaced the install's generation directory on update, and the link now
// dangles. The old code looked only at /usr/local/bin/hap: it never saw this
// link, offered to "create" one there, and failed with EACCES.
func TestInstallRepointsALinkLeftByAHerdr095Update(t *testing.T) {
	sb := newSandbox(t)
	t.Setenv("PATH", sb.userDir+string(os.PathListSeparator)+sb.systemDir)
	link := symlink(t, sb.install095("84692-0"), filepath.Join(sb.userDir, "hap")) // generation removed by the update
	current := writeExe(t, sb.install095("2217958-0"))

	located := locate(current)
	if located.Path != link || located.State != Stale || !located.OnPath {
		t.Fatalf("locate = %+v, want the dangling link %s reported stale and on PATH", located, link)
	}

	got, err := install(current)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if got.Path != link || got.State != Stale {
		t.Errorf("install reported %+v, want the pre-change state of %s (stale)", got, link)
	}
	resolvesTo(t, link, current)
	if _, err := os.Lstat(filepath.Join(sb.systemDir, "hap")); !os.IsNotExist(err) {
		t.Errorf("a second link was created in the system directory (lstat err = %v)", err)
	}
}

// A TUI pane inherits herdr's PATH, not the login shell's, so the operator's
// link can sit in a directory this process does not have on PATH at all.
func TestPathFindsALinkInTheUserBinDirOffPath(t *testing.T) {
	sb := newSandbox(t)
	link := symlink(t, sb.install095("1-0"), filepath.Join(sb.userDir, "hap"))
	current := writeExe(t, sb.install095("2-0"))

	got := locate(current)
	if got.Path != link || got.State != Stale {
		t.Fatalf("locate = %+v, want the link at %s", got, link)
	}
	if got.OnPath {
		t.Error("OnPath = true for a directory that is not in PATH")
	}
}

// An existing link wins wherever it is on PATH — including a directory this
// package would never have chosen itself.
func TestPathFindsALinkAnywhereOnPath(t *testing.T) {
	sb := newSandbox(t)
	current := writeExe(t, sb.install095("2-0"))
	link := symlink(t, current, filepath.Join(sb.pathDir, "hap"))

	if got := locate(current); got.Path != link || got.State != Current {
		t.Fatalf("locate = %+v, want the current link at %s", got, link)
	}
}

// The control for the two tests above: a `hap` that is not ours is skipped,
// not claimed, so the proposed location falls through to the default.
func TestPathSkipsAHapThatIsNotOurs(t *testing.T) {
	sb := newSandbox(t)
	current := writeExe(t, sb.install095("2-0"))
	other := writeExe(t, filepath.Join(sb.root, "elsewhere", "hap"))
	extra := filepath.Join(sb.root, "extra-bin")
	t.Setenv("PATH", sb.pathDir+string(os.PathListSeparator)+extra)
	writeExe(t, filepath.Join(sb.pathDir, "hap"))  // a regular file
	symlink(t, other, filepath.Join(extra, "hap")) // someone else's live binary

	want := filepath.Join(sb.systemDir, "hap")
	if got := Path(current); got != want {
		t.Fatalf("Path = %q, want the default %q", got, want)
	}
}

func TestDefaultLocationFollowsWritability(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write a read-only directory, so the unwritable case cannot be staged")
	}
	sb := newSandbox(t)
	current := writeExe(t, sb.install095("2-0"))

	if got, want := Path(current), filepath.Join(sb.systemDir, "hap"); got != want {
		t.Fatalf("writable system dir: Path = %q, want %q", got, want)
	}

	// A per-user herdr install: /usr/local/bin is root's.
	if err := os.Chmod(sb.systemDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sb.systemDir, 0o755) })
	want := filepath.Join(sb.userDir, "hap")
	if got := Path(current); got != want {
		t.Fatalf("unwritable system dir: Path = %q, want %q", got, want)
	}

	// ~/.local/bin does not exist yet in this sandbox; creating the link
	// creates it, and reports that the directory is not on PATH.
	got, err := install(current)
	if err != nil {
		t.Fatalf("install into a missing ~/.local/bin: %v", err)
	}
	if got.Path != want || got.State != Absent || got.OnPath {
		t.Errorf("install reported %+v, want %s created (absent before) and off PATH", got, want)
	}
	resolvesTo(t, want, current)
}

func TestHeal(t *testing.T) {
	type setup struct {
		sb      sandbox
		current string
	}
	tests := []struct {
		name string
		// arrange returns the link under test ("" when the case has none).
		arrange       func(t *testing.T, s setup) string
		wantPath      bool // Heal reports the link
		wantRepointed bool
	}{
		{
			name: "dangling link into the plugin tree is repointed",
			arrange: func(t *testing.T, s setup) string {
				return symlink(t, s.sb.install095("1-0"), filepath.Join(s.sb.pathDir, "hap"))
			},
			wantPath: true, wantRepointed: true,
		},
		{
			name: "live link to a previous plugin install is repointed",
			arrange: func(t *testing.T, s setup) string {
				old := writeExe(t, filepath.Join(s.sb.plugins, "github", "herd-auto-prompter-0.9.70", "bin", "hap"))
				return symlink(t, old, filepath.Join(s.sb.pathDir, "hap"))
			},
			wantPath: true, wantRepointed: true,
		},
		{
			name: "link already on the new install is left alone",
			arrange: func(t *testing.T, s setup) string {
				return symlink(t, s.current, filepath.Join(s.sb.pathDir, "hap"))
			},
			wantPath: true,
		},
		{
			// Install would repoint this one, on the operator's confirmation.
			// Unattended, a dead link to somewhere that was never herdr's is
			// not ours to claim.
			name: "dangling link to anywhere else is not touched",
			arrange: func(t *testing.T, s setup) string {
				return symlink(t, filepath.Join(s.sb.root, "removed-worktree", "bin", "hap"), filepath.Join(s.sb.pathDir, "hap"))
			},
		},
		{
			name: "live link to someone else's binary is not touched",
			arrange: func(t *testing.T, s setup) string {
				other := writeExe(t, filepath.Join(s.sb.root, "elsewhere", "hap"))
				return symlink(t, other, filepath.Join(s.sb.pathDir, "hap"))
			},
		},
		{
			name:    "no link is never created",
			arrange: func(*testing.T, setup) string { return "" },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sb := newSandbox(t)
			s := setup{sb: sb, current: writeExe(t, sb.install095("2-0"))}
			link := tt.arrange(t, s)
			var before string
			if link != "" {
				before, _ = os.Readlink(link)
			}

			path, repointed, err := Heal(s.current)
			if err != nil {
				t.Fatalf("Heal: %v", err)
			}
			if repointed != tt.wantRepointed {
				t.Errorf("repointed = %v, want %v", repointed, tt.wantRepointed)
			}
			if (path != "") != tt.wantPath || (tt.wantPath && path != link) {
				t.Errorf("path = %q, want reported=%v (link %q)", path, tt.wantPath, link)
			}
			switch {
			case tt.wantRepointed:
				resolvesTo(t, link, s.current)
			case link != "":
				if after, _ := os.Readlink(link); after != before {
					t.Errorf("link target changed from %q to %q", before, after)
				}
			}
			// Heal never creates: whatever it did, no directory gained a link.
			for _, dir := range []string{sb.pathDir, sb.systemDir, sb.userDir} {
				p := filepath.Join(dir, "hap")
				if p == link {
					continue
				}
				if _, err := os.Lstat(p); !os.IsNotExist(err) {
					t.Errorf("%s exists after Heal (lstat err = %v)", p, err)
				}
			}
		})
	}
}

// The structural guarantee behind the CLI's registry sweep and `hap update`'s
// tests: outside this package's sandbox, a test binary cannot rewrite the
// operator's link, whatever the caller forgot to stub.
func TestMutatingEntryPointsRefuseInsideATestBinary(t *testing.T) {
	sb := newSandbox(t)
	allowUnderTest = false // newSandbox restores it
	link := symlink(t, sb.install095("1-0"), filepath.Join(sb.pathDir, "hap"))
	current := writeExe(t, sb.install095("2-0"))

	if _, err := Install(); !errors.Is(err, errUnderTest) {
		t.Errorf("Install err = %v, want the test refusal", err)
	}
	if path, repointed, err := Heal(current); !errors.Is(err, errUnderTest) || path != "" || repointed {
		t.Errorf("Heal = (%q, %v, %v), want the test refusal and nothing reported", path, repointed, err)
	}
	if got, _ := os.Readlink(link); got != sb.install095("1-0") {
		t.Errorf("link was rewritten to %q", got)
	}
}

func TestEnsureCreatesAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "hap-current")
	target := filepath.Join(dir, "bin", "hap")
	if err := os.WriteFile(source, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := Ensure(source, target); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	linked, err := os.Readlink(target)
	if err != nil {
		t.Fatal(err)
	}
	// Ensure resolves the source through EvalSymlinks before
	// linking, so the expected value must resolve it the same way. On macOS a
	// t.TempDir() path lives under the /var → /private/var symlink, so a plain
	// filepath.Abs(source) would mismatch the resolved link target (no-op on
	// Linux, where the temp dir has no such indirection).
	resolved, err := filepath.EvalSymlinks(source)
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.Abs(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if linked != want {
		t.Fatalf("symlink source = %q, want currently running binary %q", linked, want)
	}
	if err := Ensure(source, target); err != nil {
		t.Fatalf("same symlink should be idempotent: %v", err)
	}
}

func TestEnsureRefusesExistingPath(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "hap-current")
	target := filepath.Join(dir, "hap")
	if err := os.WriteFile(source, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("keep me"), 0o755); err != nil {
		t.Fatal(err)
	}

	err := Ensure(source, target)
	if err == nil || !strings.Contains(err.Error(), "not a symlink") {
		t.Fatalf("expected refusal for existing file, got %v", err)
	}
	got, readErr := os.ReadFile(target)
	if readErr != nil || string(got) != "keep me" {
		t.Fatalf("existing target was changed: content=%q err=%v", got, readErr)
	}
}

func TestEnsureRefusesDifferentLink(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "hap-current")
	other := filepath.Join(dir, "hap-other")
	target := filepath.Join(dir, "hap")
	for _, path := range []string{source, other} {
		if err := os.WriteFile(path, []byte("binary"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(other, target); err != nil {
		t.Fatal(err)
	}

	err := Ensure(source, target)
	if err == nil || !strings.Contains(err.Error(), "different target") {
		t.Fatalf("expected refusal for unrelated symlink, got %v", err)
	}
	linked, readErr := os.Readlink(target)
	if readErr != nil || linked != other {
		t.Fatalf("existing symlink was changed: target=%q err=%v", linked, readErr)
	}
}

// A plugin upgrade installs the new release in a new directory and removes the
// old one, leaving this shortcut dangling. Refusing to touch it (the original
// behaviour) left the operator's `hap` command permanently broken with no
// in-app remedy, so a dead link is now repointed.
func TestEnsureRepointsDanglingLink(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "new-install", "bin", "hap")
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "hap")
	if err := os.Symlink(filepath.Join(dir, "removed-install", "bin", "hap"), target); err != nil {
		t.Fatal(err)
	}

	if err := Ensure(source, target); err != nil {
		t.Fatalf("dangling link should be repointed, got %v", err)
	}
	linked, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatalf("repointed link does not resolve: %v", err)
	}
	want, err := filepath.EvalSymlinks(source)
	if err != nil {
		t.Fatal(err)
	}
	if linked != want {
		t.Fatalf("symlink resolves to %q, want the running binary %q", linked, want)
	}
}

// An upgrade can also leave the link pointing at a still-present OLD plugin
// install. That is a hap we put there, so it is superseded — unlike an
// unrelated binary, which the refusal test above protects.
func TestEnsureRepointsPreviousPluginInstall(t *testing.T) {
	dir := t.TempDir()
	// Ownership is anchored at THIS machine's install root, so the test has to
	// place its fake installs inside it.
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	source := filepath.Join(dir, "config", "herdr", "plugins", "github", "herd-auto-prompter-0.4.51", "bin", "hap")
	old := filepath.Join(dir, "config", "herdr", "plugins", "github", "herd-auto-prompter-0.4.50", "bin", "hap")
	for _, path := range []string{source, old} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("binary"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(dir, "hap")
	if err := os.Symlink(old, target); err != nil {
		t.Fatal(err)
	}

	if err := Ensure(source, target); err != nil {
		t.Fatalf("link to a previous plugin install should be repointed, got %v", err)
	}
	linked, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(source)
	if err != nil {
		t.Fatal(err)
	}
	if linked != want {
		t.Fatalf("symlink resolves to %q, want %q", linked, want)
	}
}

// The repoint must never leave the path empty: a shell resolving `hap` while
// it happens has to see either the old link or the new one.
func TestRepointSymlinkIsAtomic(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "hap-new")
	if err := os.WriteFile(source, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "hap")
	if err := os.Symlink(filepath.Join(dir, "gone"), target); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- repointSymlink(source, target) }()
	// Poll Lstat throughout: rename(2) replaces the entry in one step, so the
	// name must resolve at every observation.
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("repointSymlink: %v", err)
			}
			if _, err := os.Lstat(target); err != nil {
				t.Fatalf("target missing after repoint: %v", err)
			}
			return
		default:
			if _, err := os.Lstat(target); err != nil {
				t.Fatalf("target disappeared mid-repoint: %v", err)
			}
		}
	}
}

func TestClassify(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "hap-current")
	other := filepath.Join(dir, "hap-other")
	for _, path := range []string{source, other} {
		if err := os.WriteFile(path, []byte("binary"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := func(t *testing.T, name, dest string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.Symlink(dest, path); err != nil {
			t.Fatal(err)
		}
		return path
	}
	plainFile := filepath.Join(dir, "not-a-link")
	if err := os.WriteFile(plainFile, []byte("keep me"), 0o755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		target  string
		want    State
		wantErr bool
	}{
		{"absent", filepath.Join(dir, "nothing-here"), Absent, false},
		{"already ours", link(t, "current-link", source), Current, false},
		{"dangling", link(t, "dead-link", filepath.Join(dir, "removed")), Stale, false},
		{"live foreign binary", link(t, "foreign-link", other), Foreign, true},
		{"regular file", plainFile, Foreign, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Classify(source, tt.target)
			if got != tt.want {
				t.Errorf("Classify = %v, want %v", got, tt.want)
			}
			if (err != nil) != tt.wantErr {
				t.Errorf("Classify err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
