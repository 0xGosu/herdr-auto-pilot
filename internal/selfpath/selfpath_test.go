package selfpath

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeExe creates a file with the execute bit, standing in for a hap binary.
func writeExe(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// resolved canonicalizes an expected path the way the code under test does.
// On macOS a t.TempDir() path lives under the /var → /private/var symlink, so
// comparing a raw temp path against a value that went through EvalSymlinks
// fails there while passing on Linux.
func resolved(t *testing.T, path string) string {
	t.Helper()
	out, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("resolve %s: %v", path, err)
	}
	return out
}

// isolate points every discovery fallback at an empty world so a test only
// sees the candidates it sets up itself, and clears the process-wide cache.
func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(EnvOverride, "")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("HERDR_BIN_PATH", filepath.Join(dir, "no-such-herdr"))
	t.Setenv("PATH", filepath.Join(dir, "empty-bin"))
	cacheMu.Lock()
	cached = ""
	cacheMu.Unlock()
	t.Cleanup(func() {
		cacheMu.Lock()
		cached = ""
		cacheMu.Unlock()
	})
	return dir
}

func TestResolvePrefersEnvOverride(t *testing.T) {
	dir := isolate(t)
	// Deliberately a path that does NOT exist: an explicit override is
	// authoritative, so a wrong value must surface rather than be replaced.
	want := filepath.Join(dir, "pinned", "hap")
	t.Setenv(EnvOverride, want)

	got, err := Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != want {
		t.Fatalf("Resolve = %q, want the override %q", got, want)
	}
}

func TestResolveUsesRunningBinaryWhenItStillExists(t *testing.T) {
	isolate(t)
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable unavailable: %v", err)
	}

	got, err := Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != exe {
		t.Fatalf("Resolve = %q, want the running test binary %q", got, exe)
	}
}

func TestResolveFallsBackToNewestInstallDir(t *testing.T) {
	dir := isolate(t)
	plugins := filepath.Join(dir, "config", "herdr", "plugins", "github")
	old := writeExe(t, filepath.Join(plugins, "herd-auto-prompter-0.4.50", "bin", "hap"))
	recent := writeExe(t, filepath.Join(plugins, "herd-auto-prompter-0.4.51", "bin", "hap"))
	// Version-stamped dirs sort unhelpfully once versions reach two digits, so
	// discovery goes by mtime: make the intended winner unambiguously newer.
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	got := fromInstallDirs()
	if got != recent {
		t.Fatalf("fromInstallDirs = %q, want the newest install %q", got, recent)
	}
}

// herdr 0.9.5 moved installs to <root>/github-installations/<id>-<hash>/
// <generation>/checkout. The scan knew only the older two-level layout, so on
// a current herdr it found nothing — unnoticed, because the registry lookup
// ahead of it normally answers, and fatal exactly when herdr cannot be asked.
func TestInstallDirScanFindsTheHerdr095Layout(t *testing.T) {
	dir := isolate(t)
	install := filepath.Join(dir, "config", "herdr", "plugins", "github-installations", "herd-auto-prompter-e001d0f4b256")
	old := writeExe(t, filepath.Join(install, "84692-0", "checkout", "bin", "hap"))
	recent := writeExe(t, filepath.Join(install, "2217958-0", "checkout", "bin", "hap"))
	// Generations are not ordered by name ("84692-0" sorts after "2217958-0"),
	// so the newest binary has to win by mtime here too.
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	if got := fromInstallDirs(); got != recent {
		t.Fatalf("fromInstallDirs = %q, want the newest generation %q", got, recent)
	}
	if got := Installed(); got != recent {
		t.Fatalf("Installed = %q, want %q", got, recent)
	}
}

// A machine upgraded across the layout change can hold both trees for a
// while; the newest binary wins whichever layout it is in.
func TestInstallDirScanPicksTheNewestAcrossLayouts(t *testing.T) {
	dir := isolate(t)
	plugins := filepath.Join(dir, "config", "herdr", "plugins")
	legacy := writeExe(t, filepath.Join(plugins, "github", "herd-auto-prompter-0.9.70", "bin", "hap"))
	current := writeExe(t, filepath.Join(plugins, "github-installations", "herd-auto-prompter-e001d0f4b256", "1-0", "checkout", "bin", "hap"))
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(legacy, past, past); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if got := fromInstallDirs(); got != current {
		t.Fatalf("fromInstallDirs = %q, want %q", got, current)
	}

	if err := os.Chtimes(current, past.Add(-time.Hour), past.Add(-time.Hour)); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if got := fromInstallDirs(); got != legacy {
		t.Fatalf("fromInstallDirs = %q, want %q once it is the newer one", got, legacy)
	}
}

func TestResolveReplacementSkipsNonExecutableCandidate(t *testing.T) {
	dir := isolate(t)
	plugins := filepath.Join(dir, "config", "herdr", "plugins", "github")
	// A downloaded-but-not-yet-chmod'ed binary must not be handed out as the
	// successor: spawning it would fail with EACCES.
	noexec := filepath.Join(plugins, "herd-auto-prompter-0.4.51", "bin", "hap")
	writeExe(t, noexec)
	if err := os.Chmod(noexec, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	if _, err := resolveReplacement(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("resolveReplacement err = %v, want ErrNotFound", err)
	}
}

func TestResolveReplacementFindsPATHShortcut(t *testing.T) {
	dir := isolate(t)
	bin := filepath.Join(dir, "path-bin")
	want := writeExe(t, filepath.Join(bin, "hap"))
	t.Setenv("PATH", bin)

	got, err := resolveReplacement()
	if err != nil {
		t.Fatalf("resolveReplacement: %v", err)
	}
	if got != resolved(t, want) {
		t.Fatalf("resolveReplacement = %q, want %q", got, want)
	}
}

func TestResolveReplacementCacheRevalidates(t *testing.T) {
	dir := isolate(t)
	bin := filepath.Join(dir, "path-bin")
	first := writeExe(t, filepath.Join(bin, "hap"))
	t.Setenv("PATH", bin)

	if got, err := resolveReplacement(); err != nil || got != resolved(t, first) {
		t.Fatalf("first resolveReplacement = %q, %v; want %q", got, err, first)
	}
	// A second upgrade removes the cached answer too. The cache must not
	// outlive the file it names.
	if err := os.Remove(first); err != nil {
		t.Fatalf("remove: %v", err)
	}
	second := writeExe(t, filepath.Join(dir, "path-bin2", "hap"))
	t.Setenv("PATH", filepath.Join(dir, "path-bin2"))

	got, err := resolveReplacement()
	if err != nil {
		t.Fatalf("second resolveReplacement: %v", err)
	}
	if got != resolved(t, second) {
		t.Fatalf("second resolveReplacement = %q, want the new binary %q", got, second)
	}
}

func TestResolveReplacementReportsNothingFound(t *testing.T) {
	isolate(t)
	if _, err := resolveReplacement(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("resolveReplacement err = %v, want ErrNotFound", err)
	}
}

func TestMissing(t *testing.T) {
	dir := t.TempDir()
	live := writeExe(t, filepath.Join(dir, "hap"))
	tests := []struct {
		name string
		path string
		want bool
	}{
		{"live binary", live, false},
		{"removed binary", filepath.Join(dir, "gone"), true},
		{"unknown path", "", false},
		{"directory", dir, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Missing(tt.path); got != tt.want {
				t.Fatalf("Missing(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestSameResolvesSymlinks(t *testing.T) {
	dir := t.TempDir()
	real := writeExe(t, filepath.Join(dir, "plugin", "bin", "hap"))
	link := filepath.Join(dir, "usr-local-hap")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	other := writeExe(t, filepath.Join(dir, "other", "bin", "hap"))

	if !Same(link, real) {
		t.Fatal("Same(symlink, target) = false; a PATH shortcut must not read as a different binary")
	}
	if Same(real, other) {
		t.Fatal("Same(distinct binaries) = true")
	}
	if Same(real, filepath.Join(dir, "gone")) {
		t.Fatal("Same(live, missing) = true; an unresolvable path must compare false")
	}
	if Same("", "") {
		t.Fatal("Same(\"\", \"\") = true; an unknown path must compare false")
	}
}

func TestPluginRootIsTwoLevelsAboveTheBinary(t *testing.T) {
	dir := isolate(t)
	root := filepath.Join(dir, "plugin")
	t.Setenv(EnvOverride, writeExe(t, filepath.Join(root, "bin", "hap")))

	if got := PluginRoot(); got != resolved(t, root) {
		t.Fatalf("PluginRoot = %q, want %q", got, root)
	}
}

// TestInstalledIgnoresTheRunningBinary is the `hap update` contract: right
// after an install, the running process and the operator's PATH shortcut are
// both the OLD build, so Installed must report what herdr installed instead —
// otherwise `hap daemon --ensure` hands the daemon back to the binary the
// upgrade just replaced.
func TestInstalledIgnoresTheRunningBinary(t *testing.T) {
	dir := isolate(t)
	plugins := filepath.Join(dir, "config", "herdr", "plugins", "github")
	newBin := writeExe(t, filepath.Join(plugins, "herd-auto-prompter-0.5.2", "bin", "hap"))

	// A PATH shortcut pointing at the previous build must not win.
	pathBin := writeExe(t, filepath.Join(dir, "path-bin", "hap"))
	t.Setenv("PATH", filepath.Dir(pathBin))

	got := Installed()
	if got != newBin {
		t.Fatalf("Installed = %q, want the installed binary %q", got, newBin)
	}
}

// TestInstalledReportsNothingWhenUndiscoverable keeps the caller's fallback
// path honest: a linked working tree has no install dir at all.
func TestInstalledReportsNothingWhenUndiscoverable(t *testing.T) {
	isolate(t)
	if got := Installed(); got != "" {
		t.Errorf("Installed = %q, want empty when nothing is installed", got)
	}
}

// fakeHerdr installs a herdr that answers `plugin list --json` the way herdr
// 0.9.5 does (shape captured live), naming root as the plugin's install.
func fakeHerdr(t *testing.T, dir, root string) {
	t.Helper()
	body := `{"id":"cli:plugin","result":{"plugins":[{"enabled":true,"plugin_id":"other-plugin","plugin_root":"/elsewhere"},` +
		`{"enabled":true,"plugin_id":"herd-auto-prompter","plugin_root":"` + root + `",` +
		`"source":{"kind":"github","managed_path":"` + root + `"},"version":"0.9.77"}],"type":"plugin_list"}}`
	bin := filepath.Join(dir, "herdr")
	// printf is a shell builtin: isolate() empties PATH, so no external tool
	// (cat) can be relied on.
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' '"+body+"'\n"), 0o755); err != nil {
		t.Fatalf("write fake herdr: %v", err)
	}
	t.Setenv("HERDR_BIN_PATH", bin)
}

func install095(dir, generation string) string {
	return filepath.Join(dir, "config", "herdr", "plugins", "github-installations",
		"herd-auto-prompter-e001d0f4b256", generation, "checkout")
}

func TestManaged(t *testing.T) {
	dir := isolate(t)
	plugins := filepath.Join(dir, "config", "herdr", "plugins")
	tests := []struct {
		path string
		want bool
	}{
		{filepath.Join(install095(dir, "1-0"), "bin", "hap"), true},
		{filepath.Join(plugins, "github", "herd-auto-prompter-0.9.70", "bin", "hap"), true},
		// The install root itself is not an install.
		{plugins, false},
		// A linked working tree, and a look-alike path outside the root.
		{filepath.Join(dir, "workspace", "herdr-auto-pilot", "bin", "hap"), false},
		{filepath.Join(dir, "elsewhere", "herdr", "plugins", "github", "x", "bin", "hap"), false},
		{plugins + "-backup/bin/hap", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := Managed(tt.path); got != tt.want {
			t.Errorf("Managed(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

// TestSupersededBy pins the herdr 0.9.5 question: a replaced installation
// stays on disk, so "my binary exists" no longer means "I am the installed
// build". Only herdr's registry can say, and only a managed install can lose.
func TestSupersededBy(t *testing.T) {
	t.Run("an older retained generation is superseded by the registered one", func(t *testing.T) {
		dir := isolate(t)
		old := writeExe(t, filepath.Join(install095(dir, "84692-0"), "bin", "hap"))
		current := writeExe(t, filepath.Join(install095(dir, "2217958-0"), "bin", "hap"))
		fakeHerdr(t, dir, install095(dir, "2217958-0"))

		if got := SupersededBy(old); got != current {
			t.Fatalf("SupersededBy(old) = %q, want %q", got, current)
		}
		if got := SupersededBy(current); got != "" {
			t.Fatalf("SupersededBy(current) = %q, want none — it is the registered install", got)
		}
		if got := Registered(); got != current {
			t.Fatalf("Registered = %q, want %q", got, current)
		}
	})

	// The dev loop: a linked working tree taking the daemon over from a
	// registered release is deliberate, never "superseded".
	t.Run("a binary outside the install tree is never superseded", func(t *testing.T) {
		dir := isolate(t)
		dev := writeExe(t, filepath.Join(dir, "workspace", "herdr-auto-pilot", "bin", "hap"))
		writeExe(t, filepath.Join(install095(dir, "1-0"), "bin", "hap"))
		fakeHerdr(t, dir, install095(dir, "1-0"))

		if got := SupersededBy(dev); got != "" {
			t.Fatalf("SupersededBy(dev tree) = %q, want none", got)
		}
	})

	// Unknown is not "superseded": with herdr unreachable, or naming an
	// install whose binary is not there, the caller carries on as itself. The
	// install-tree scan is deliberately NOT consulted — a sibling generation
	// may be a build herdr aborted before registering.
	t.Run("nothing is concluded when the registry cannot say", func(t *testing.T) {
		dir := isolate(t) // HERDR_BIN_PATH names no herdr
		old := writeExe(t, filepath.Join(install095(dir, "1-0"), "bin", "hap"))
		writeExe(t, filepath.Join(install095(dir, "2-0"), "bin", "hap"))
		if got := SupersededBy(old); got != "" {
			t.Fatalf("herdr unreachable: SupersededBy = %q, want none", got)
		}

		fakeHerdr(t, dir, install095(dir, "3-0")) // registered, but no binary there
		if got := SupersededBy(old); got != "" {
			t.Fatalf("registered binary missing: SupersededBy = %q, want none", got)
		}
		if got := Registered(); got != "" {
			t.Fatalf("Registered = %q, want none for a missing binary", got)
		}
	})
}
