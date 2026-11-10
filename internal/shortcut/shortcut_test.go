package shortcut

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/selfpath"
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
	// No test may ask the operator's real herdr which install is registered.
	t.Setenv("HERDR_BIN_PATH", filepath.Join(root, "no-such-herdr"))
	t.Setenv(selfpath.EnvOverride, "")
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

// readOnly makes dir unwritable for the test — what /usr/local/bin is to an
// ordinary user on macOS and on any per-user herdr install.
func readOnly(t *testing.T, dir string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root can write a read-only directory, so the unwritable case cannot be staged")
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
}

// TestAStaleLinkInAnUnwritableDirectoryIsSteppedOver is the second failure
// report for this feature: /usr/local/bin/hap exists from an earlier install
// (made with sudo, or when the directory was writable), is now stale, and the
// operator cannot write /usr/local/bin. Locating it as "the link" made the TUI
// row and `hap link` fail with "permission denied" on every attempt — nothing
// they could press would ever give them a working `hap`.
func TestAStaleLinkInAnUnwritableDirectoryIsSteppedOver(t *testing.T) {
	for _, tc := range []struct {
		name string
		// retained: the old install is still on disk (herdr ≥0.9.5 keeps it
		// until its server exits), so the stale link still RESOLVES.
		retained bool
	}{
		{"dangling", false},
		{"still resolving to a retained old install", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sb := newSandbox(t)
			t.Setenv("PATH", sb.systemDir+string(os.PathListSeparator)+sb.userDir)
			old := sb.install095("1-0")
			if tc.retained {
				writeExe(t, old)
			}
			stale := symlink(t, old, filepath.Join(sb.systemDir, "hap"))
			current := writeExe(t, sb.install095("2-0"))
			readOnly(t, sb.systemDir)

			want := filepath.Join(sb.userDir, "hap")
			got, err := install(current)
			if err != nil {
				t.Fatalf("install must not fail on a link it cannot write: %v", err)
			}
			if got.Path != want || got.State != Absent || got.Blocked != stale {
				t.Fatalf("install reported %+v, want %s created with %s reported as blocked", got, want, stale)
			}
			// systemDir precedes userDir on this PATH, so the old link wins
			// in a shell exactly when it still resolves; a dead one is
			// skipped by the shell's search and shadows nothing.
			if got.Shadows != tc.retained {
				t.Errorf("Shadows = %v, want %v", got.Shadows, tc.retained)
			}
			resolvesTo(t, want, current)
			if after, _ := os.Readlink(stale); after != old {
				t.Errorf("the unwritable link changed to %q", after)
			}

			// Second press: the link made above is ours, writable and current.
			// The blocked one is still reported — it still needs removing.
			again := locate(current)
			if again.Path != want || again.State != Current || again.Blocked != stale {
				t.Errorf("locate after the fix = %+v, want %s current, %s still blocked", again, want, stale)
			}
		})
	}
}

// The reported machine: ~/.local/bin (where herdr lives) comes BEFORE
// /usr/local/bin on PATH, and the old link still resolves — to a legacy
// plugins/github/ install that herdr 0.9.5 never cleans up. The new link goes
// beside herdr and wins; the old one is litter, not something the operator
// must fix with sudo.
func TestABlockedLinkLaterOnPathDoesNotShadow(t *testing.T) {
	sb := newSandbox(t)
	t.Setenv("PATH", sb.userDir+string(os.PathListSeparator)+sb.systemDir)
	legacy := writeExe(t, filepath.Join(sb.plugins, "github", "herd-auto-prompter-e001d0f4b256", "bin", "hap"))
	stale := symlink(t, legacy, filepath.Join(sb.systemDir, "hap"))
	writeExe(t, filepath.Join(sb.userDir, "herdr"))
	t.Setenv("HERDR_BIN_PATH", filepath.Join(sb.userDir, "herdr"))
	current := writeExe(t, sb.install095("73398-0"))
	readOnly(t, sb.systemDir)

	got, err := install(current)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	want := filepath.Join(sb.userDir, "hap")
	if got.Path != want || !got.OnPath || got.Blocked != stale {
		t.Fatalf("install reported %+v, want %s (on PATH) with %s blocked", got, want, stale)
	}
	if got.Shadows {
		t.Error("Shadows = true for a blocked link that comes later on PATH")
	}
	resolvesTo(t, want, current)
}

// The control: an unwritable directory only matters when something has to be
// WRITTEN. A link there that already points at this binary is the link.
func TestACurrentLinkInAnUnwritableDirectoryIsStillTheLink(t *testing.T) {
	sb := newSandbox(t)
	t.Setenv("PATH", sb.systemDir)
	current := writeExe(t, sb.install095("2-0"))
	link := symlink(t, current, filepath.Join(sb.systemDir, "hap"))
	readOnly(t, sb.systemDir)

	got, err := install(current)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if got.Path != link || got.State != Current || got.Blocked != "" {
		t.Fatalf("install reported %+v, want the existing link %s, current, nothing blocked", got, link)
	}
	if _, err := os.Lstat(filepath.Join(sb.userDir, "hap")); !os.IsNotExist(err) {
		t.Errorf("a second link was created (lstat err = %v)", err)
	}
}

// A writable link of ours further along PATH is repointed in preference to
// creating a third one.
func TestAWritableLinkBeatsTheDefaultWhenAnotherIsBlocked(t *testing.T) {
	sb := newSandbox(t)
	t.Setenv("PATH", sb.systemDir+string(os.PathListSeparator)+sb.pathDir)
	blocked := symlink(t, sb.install095("1-0"), filepath.Join(sb.systemDir, "hap"))
	mine := symlink(t, sb.install095("1-0"), filepath.Join(sb.pathDir, "hap"))
	current := writeExe(t, sb.install095("2-0"))
	readOnly(t, sb.systemDir)

	got := locate(current)
	if got.Path != mine || got.State != Stale || got.Blocked != blocked {
		t.Fatalf("locate = %+v, want %s stale with %s blocked", got, mine, blocked)
	}

	// The unattended repair after `hap update` makes the same choice…
	path, repointed, err := Heal(current)
	if err != nil || !repointed || path != mine {
		t.Fatalf("Heal = (%q, %v, %v), want %s repointed", path, repointed, err, mine)
	}
	resolvesTo(t, mine, current)
}

// …and with ONLY a blocked link it changes nothing and says which one, so
// `hap update` can tell the operator to run `hap link`.
func TestHealReportsALinkItCannotWrite(t *testing.T) {
	sb := newSandbox(t)
	t.Setenv("PATH", sb.systemDir)
	old := sb.install095("1-0")
	blocked := symlink(t, old, filepath.Join(sb.systemDir, "hap"))
	current := writeExe(t, sb.install095("2-0"))
	readOnly(t, sb.systemDir)

	path, repointed, err := Heal(current)
	if err == nil || repointed || path != blocked {
		t.Fatalf("Heal = (%q, %v, %v), want %s reported, unchanged, with an error", path, repointed, err, blocked)
	}
	if !strings.Contains(err.Error(), "not writable") {
		t.Errorf("err = %v, want it to say the directory is not writable", err)
	}
	if after, _ := os.Readlink(blocked); after != old {
		t.Errorf("the unwritable link changed to %q", after)
	}
	if _, err := os.Lstat(filepath.Join(sb.userDir, "hap")); !os.IsNotExist(err) {
		t.Errorf("Heal created a link (lstat err = %v)", err)
	}
}

// With /usr/local/bin unwritable, a new link belongs where it works with no
// further step: the directory herdr itself runs from is the operator's own and
// is on their PATH. macOS has no ~/.local/bin on PATH by default, so falling
// straight back to it there trades one manual step for another.
func TestNewLinkGoesBesideHerdrWhenThatIsOnPath(t *testing.T) {
	sb := newSandbox(t)
	current := writeExe(t, sb.install095("2-0"))
	brew := filepath.Join(sb.root, "homebrew", "bin")
	herdr := writeExe(t, filepath.Join(brew, "herdr"))
	readOnly(t, sb.systemDir)

	t.Run("on PATH: used", func(t *testing.T) {
		t.Setenv("PATH", sb.systemDir+string(os.PathListSeparator)+brew)
		got := locate(current)
		if want := filepath.Join(brew, "hap"); got.Path != want || !got.OnPath {
			t.Fatalf("locate = %+v, want %s, on PATH", got, want)
		}
	})

	// HERDR_BIN_PATH may name a resolved, version-stamped location (a
	// Homebrew Cellar path): a directory nobody has on PATH is no better
	// than ~/.local/bin, and worse for being replaced on the next upgrade.
	t.Run("named by HERDR_BIN_PATH but not on PATH: not used", func(t *testing.T) {
		t.Setenv("PATH", sb.systemDir)
		t.Setenv("HERDR_BIN_PATH", herdr)
		got := locate(current)
		if want := filepath.Join(sb.userDir, "hap"); got.Path != want {
			t.Fatalf("locate = %+v, want the ~/.local/bin fallback %s", got, want)
		}
	})

	t.Run("on PATH but not writable: not used", func(t *testing.T) {
		t.Setenv("PATH", sb.systemDir+string(os.PathListSeparator)+brew)
		readOnly(t, brew)
		got := locate(current)
		if want := filepath.Join(sb.userDir, "hap"); got.Path != want {
			t.Fatalf("locate = %+v, want the ~/.local/bin fallback %s", got, want)
		}
	})
}

// The TUI shows an action's error on one truncated line. os.Symlink leads
// with both paths — "symlink <long install path> <target>: permission denied"
// — so the reason was exactly the part cut off ("…: symlink /Users…").
func TestLinkErrorsLeadWithTheReason(t *testing.T) {
	sb := newSandbox(t)
	current := writeExe(t, sb.install095("2-0"))
	target := symlink(t, sb.install095("1-0"), filepath.Join(sb.systemDir, "hap"))
	readOnly(t, sb.systemDir)

	// Ensure is the low-level call: asked for THIS target, it still tries.
	err := Ensure(current, target)
	if err == nil {
		t.Fatal("repointing a link in a read-only directory succeeded")
	}
	want := "repoint " + target + ": permission denied"
	if err.Error() != want {
		t.Errorf("err = %q, want %q", err, want)
	}
	if !errors.Is(err, os.ErrPermission) {
		t.Errorf("err = %v, want it to remain a permission error", err)
	}
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

// registerWithHerdr installs a fake herdr whose registry names root as the
// plugin's install, and makes self the binary this process "is".
func (sb sandbox) registerWithHerdr(t *testing.T, self, registered string) {
	t.Helper()
	root := filepath.Dir(filepath.Dir(registered))
	// printf is a shell builtin; the sandbox PATH holds no external tools.
	script := "#!/bin/sh\nprintf '%s\\n' '" +
		`{"result":{"plugins":[{"plugin_id":"herd-auto-prompter","plugin_root":"` + root + `"}]}}` + "'\n"
	herdr := filepath.Join(sb.root, "herdr")
	if err := os.WriteFile(herdr, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_BIN_PATH", herdr)
	t.Setenv(selfpath.EnvOverride, self)
}

// TestInstallFromASupersededBuildLinksTheRegisteredInstall: herdr ≥0.9.5 keeps
// a replaced install runnable, so a TUI left open across `hap update` is the
// OLD build. `hap update` has already moved the link to the new install — and
// to the old build that link reads as Stale ("a previous version of
// ourselves"). Acting on it must not move `hap` BACK to the superseded build.
func TestInstallFromASupersededBuildLinksTheRegisteredInstall(t *testing.T) {
	sb := newSandbox(t)
	old := writeExe(t, sb.install095("1-0"))
	current := writeExe(t, sb.install095("2-0"))
	link := symlink(t, current, filepath.Join(sb.pathDir, "hap")) // already healed
	sb.registerWithHerdr(t, old, current)

	// The premise: seen from the old build, the healed link looks stale.
	if got := Locate(); got.Path != link || got.State != Stale {
		t.Fatalf("premise: Locate from the old build = %+v, want %s stale", got, link)
	}

	got, err := Install()
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	resolvesTo(t, link, current)
	if got.Source != current || got.State != Current {
		t.Errorf("Install reported %+v, want the registered install %s, already linked", got, current)
	}
}

// The control: the build herdr runs links ITSELF, superseding the old link.
func TestInstallFromTheRegisteredBuildLinksItself(t *testing.T) {
	sb := newSandbox(t)
	old := writeExe(t, sb.install095("1-0"))
	current := writeExe(t, sb.install095("2-0"))
	link := symlink(t, old, filepath.Join(sb.pathDir, "hap"))
	sb.registerWithHerdr(t, current, current)

	got, err := Install()
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	resolvesTo(t, link, current)
	if got.State != Stale {
		t.Errorf("Install reported state %v, want the link's pre-change state (stale)", got.State)
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
