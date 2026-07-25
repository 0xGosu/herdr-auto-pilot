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
