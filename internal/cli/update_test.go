package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/buildinfo"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/updatecheck"
)

// stubRunner swaps the reinstall for a fake, so no test invokes a real herdr.
func stubRunner(t *testing.T, err error) *bool {
	t.Helper()
	ran := false
	orig := updateRunner
	t.Cleanup(func() { updateRunner = orig })
	updateRunner = func(_ context.Context, _ io.Writer) error {
		ran = true
		return err
	}
	return &ran
}

// updateTestApp gives update() a cached latest version to report.
func updateTestApp(t *testing.T, cached string) *frontend.App {
	t.Helper()
	dir := t.TempDir()
	if cached != "" {
		st := updatecheck.State{
			CheckedAt:     time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC),
			LatestVersion: cached,
		}
		if err := updatecheck.Write(dir, st); err != nil {
			t.Fatal(err)
		}
	}
	return &frontend.App{
		StateDir: dir,
		FetchLatestVersion: func(context.Context) (string, error) {
			return "", errors.New("no network in tests")
		},
	}
}

// updateTestAppLive is updateTestApp with a WORKING release fetch, for the
// cases that prove the live check outranks whatever the cache says.
func updateTestAppLive(t *testing.T, cached, live string) *frontend.App {
	t.Helper()
	app := updateTestApp(t, cached)
	app.FetchLatestVersion = func(context.Context) (string, error) {
		return live, nil
	}
	return app
}

// stubInstalledVersion swaps the post-install version read-back for a fake, so
// no test executes whatever binary the host's herdr registry names.
func stubInstalledVersion(t *testing.T, v string) {
	t.Helper()
	orig := installedVersion
	t.Cleanup(func() { installedVersion = orig })
	installedVersion = func(context.Context) string { return v }
}

// stampRelease makes this binary look like a published release, which is what
// `hap update` requires before it will install anything.
func stampRelease(t *testing.T, v string) {
	t.Helper()
	orig := buildinfo.Version
	t.Cleanup(func() { buildinfo.Version = orig })
	buildinfo.Version = v
}

func TestUpdateRunsInstallAndPointsAtEnsure(t *testing.T) {
	stampRelease(t, "v0.5.1")

	ran := stubRunner(t, nil)
	var out bytes.Buffer
	if err := update(context.Background(), updateTestApp(t, "v0.5.2"), &out, nil); err != nil {
		t.Fatalf("update: %v", err)
	}
	if !*ran {
		t.Fatal("update did not run the installer")
	}
	got := out.String()
	for _, want := range []string{
		"v0.5.2",              // the version being installed
		"v0.5.1",              // the version being replaced
		"plugin install",      // the documented upgrade command
		updatecheck.Repo,      // ...aimed at this project
		"hap daemon --ensure", // the hand-over follow-up
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

// TestUpdateInstallFailureSurfaces keeps a failed reinstall from reading as a
// success — the operator must not be told to hand a daemon over to nothing.
func TestUpdateInstallFailureSurfaces(t *testing.T) {
	stampRelease(t, "v0.5.1")
	stubRunner(t, errors.New("install exploded"))
	var out bytes.Buffer
	err := update(context.Background(), updateTestApp(t, "v0.5.2"), &out, nil)
	if err == nil {
		t.Fatal("expected the install error to propagate")
	}
	if strings.Contains(out.String(), "hap daemon --ensure") {
		t.Errorf("failed install still printed the hand-over step:\n%s", out.String())
	}
}

// TestUpdateWithoutKnownVersion still upgrades: an unknown latest version only
// makes the message vaguer, it never blocks the install.
func TestUpdateWithoutKnownVersion(t *testing.T) {
	stampRelease(t, "v0.5.1")
	ran := stubRunner(t, nil)
	var out bytes.Buffer
	if err := update(context.Background(), updateTestApp(t, ""), &out, nil); err != nil {
		t.Fatalf("update: %v", err)
	}
	if !*ran {
		t.Fatal("update did not run the installer")
	}
	if !strings.Contains(out.String(), "hap daemon --ensure") {
		t.Errorf("output missing the hand-over step:\n%s", out.String())
	}
}

// TestUpdateRefusesOnDevBuildWithoutForce protects the `herdr plugin link`
// working tree: installing a release over it would replace the checkout the
// developer is running from.
func TestUpdateRefusesOnDevBuildWithoutForce(t *testing.T) {
	for _, version := range []string{"dev", "dev-20260726120000"} {
		t.Run(version, func(t *testing.T) {
			stampRelease(t, version)
			ran := stubRunner(t, nil)
			var out bytes.Buffer

			err := update(context.Background(), updateTestApp(t, "v0.5.2"), &out, nil)
			if err == nil {
				t.Fatal("expected a refusal on a non-release build")
			}
			if *ran {
				t.Fatal("refused but still ran the installer")
			}
			if !strings.Contains(err.Error(), "--force") {
				t.Errorf("refusal does not name the escape hatch: %v", err)
			}

			// --force is the operator saying they mean it.
			if err := update(context.Background(), updateTestApp(t, "v0.5.2"), &out, []string{"--force"}); err != nil {
				t.Fatalf("update --force: %v", err)
			}
			if !*ran {
				t.Error("--force did not run the installer")
			}
		})
	}
}

// TestRunHerdrInstallRefusesUnderTest is the structural guarantee that no unit
// test — in ANY package, including a future registry sweep — can reinstall the
// operator's live plugin.
func TestRunHerdrInstallRefusesUnderTest(t *testing.T) {
	var out bytes.Buffer
	if err := runHerdrInstall(context.Background(), &out); err == nil {
		t.Fatal("runHerdrInstall must refuse to execute under `go test`")
	}
	if out.Len() != 0 {
		t.Errorf("refusal still produced output: %q", out.String())
	}
}

// stubBinaries describes the install layout the follow-up step is computed
// from: what herdr now reports as installed, and what `hap` on PATH resolves
// to. Empty strings mean "not discoverable".
func stubBinaries(t *testing.T, installed, onPath string) {
	t.Helper()
	origInstalled, origLook := installedBinary, lookPath
	t.Cleanup(func() { installedBinary, lookPath = origInstalled, origLook })
	installedBinary = func() string { return installed }
	lookPath = func(string) (string, error) {
		if onPath == "" {
			return "", errors.New("hap not on PATH")
		}
		return onPath, nil
	}
}

// TestUpdatePointsEnsureAtTheNewBinary is the whole point of the follow-up
// line: this process is the OLD build and the `hap` shortcut is not repointed
// by an install, so a bare `hap daemon --ensure` can hand the daemon straight
// back to the binary that was just replaced.
func TestUpdatePointsEnsureAtTheNewBinary(t *testing.T) {
	stampRelease(t, "v0.5.1")
	stubRunner(t, nil)

	newBin := filepath.Join(t.TempDir(), "new", "bin", "hap")
	oldBin := filepath.Join(t.TempDir(), "old", "bin", "hap")
	stubBinaries(t, newBin, oldBin)

	var out bytes.Buffer
	if err := update(context.Background(), updateTestApp(t, "v0.5.2"), &out, nil); err != nil {
		t.Fatalf("update: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, newBin+" daemon --ensure") {
		t.Errorf("follow-up step does not name the newly installed binary:\n%s", got)
	}
	if strings.Contains(got, "`hap daemon --ensure`") {
		t.Errorf("follow-up step still offers the stale PATH binary:\n%s", got)
	}
	if !strings.Contains(got, "PATH") {
		t.Errorf("output does not explain why the full path is used:\n%s", got)
	}
}

// TestUpdateEnsureStepFallsBackToPlainCommand keeps the message readable when
// the absolute path adds nothing: PATH already resolves to the new binary, or
// no install dir can be discovered at all.
func TestUpdateEnsureStepFallsBackToPlainCommand(t *testing.T) {
	shared := filepath.Join(t.TempDir(), "bin", "hap")
	if err := os.MkdirAll(filepath.Dir(shared), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shared, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name      string
		installed string
		onPath    string
	}{
		{"PATH already points at the new binary", shared, shared},
		{"no install dir discoverable", "", "/usr/local/bin/hap"},
		{"nothing discoverable at all", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stampRelease(t, "v0.5.1")
			stubRunner(t, nil)
			stubBinaries(t, tc.installed, tc.onPath)

			var out bytes.Buffer
			if err := update(context.Background(), updateTestApp(t, "v0.5.2"), &out, nil); err != nil {
				t.Fatalf("update: %v", err)
			}
			if !strings.Contains(out.String(), "`hap daemon --ensure`") {
				t.Errorf("expected the plain follow-up command:\n%s", out.String())
			}
		})
	}
}

// TestUpdateLiveCheckOutranksStaleCache is the fix for the v0.6.4/v0.6.5
// mismatch: the cached record can predate a release that published minutes
// ago, so a successful live fetch must name the version — the cache is only
// the fallback for a fetch that fails.
func TestUpdateLiveCheckOutranksStaleCache(t *testing.T) {
	stampRelease(t, "v0.6.4")
	stubRunner(t, nil)
	app := updateTestAppLive(t, "v0.6.4", "v0.6.5")

	var out bytes.Buffer
	if err := update(context.Background(), app, &out, nil); err != nil {
		t.Fatalf("update: %v", err)
	}
	if !strings.Contains(out.String(), "newest known: v0.6.5") {
		t.Errorf("live check did not outrank the stale cache:\n%s", out.String())
	}
	// The successful check also refreshes the record, so the next reader
	// (the TUI header) stops naming the stale version too.
	if st, ok := updatecheck.Read(app.StateDir); !ok || st.LatestVersion != "v0.6.5" {
		t.Errorf("cache not refreshed by the live check: %+v (ok=%v)", st, ok)
	}
}

// TestUpdateFallsBackToCacheWhenFetchFails keeps the message useful offline:
// a dead network costs freshness, never the version hint entirely.
func TestUpdateFallsBackToCacheWhenFetchFails(t *testing.T) {
	stampRelease(t, "v0.5.1")
	stubRunner(t, nil)
	var out bytes.Buffer
	// updateTestApp's fetch always errors, so the cache is all there is.
	if err := update(context.Background(), updateTestApp(t, "v0.5.2"), &out, nil); err != nil {
		t.Fatalf("update: %v", err)
	}
	if !strings.Contains(out.String(), "newest known: v0.5.2") {
		t.Errorf("failed fetch did not fall back to the cached version:\n%s", out.String())
	}
}

// TestUpdateReportsTheVersionThatActuallyInstalled pins the closing line to
// the installed binary's own report, not to the expectation printed up front.
func TestUpdateReportsTheVersionThatActuallyInstalled(t *testing.T) {
	stampRelease(t, "v0.6.4")
	stubRunner(t, nil)
	stubInstalledVersion(t, "v0.6.5")

	var out bytes.Buffer
	if err := update(context.Background(), updateTestAppLive(t, "", "v0.6.5"), &out, nil); err != nil {
		t.Fatalf("update: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "installed v0.6.5") {
		t.Errorf("closing line does not report the read-back version:\n%s", got)
	}
	if strings.Contains(got, "note:") {
		t.Errorf("expected no asset-fallback note when installed == latest:\n%s", got)
	}
}

// TestUpdateAssetFallbackNamesWhatLanded covers the publish gap: the checkout's
// manifest names a release whose assets have not published, install.sh falls
// back to the previous release's binaries, and the closing line must report
// THAT version — plus a note that the newer one is worth a retry.
func TestUpdateAssetFallbackNamesWhatLanded(t *testing.T) {
	stampRelease(t, "v0.6.3")
	stubRunner(t, nil)
	stubInstalledVersion(t, "v0.6.4")

	var out bytes.Buffer
	if err := update(context.Background(), updateTestAppLive(t, "", "v0.6.5"), &out, nil); err != nil {
		t.Fatalf("update: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "installed v0.6.4") {
		t.Errorf("closing line does not report what actually landed:\n%s", got)
	}
	if !strings.Contains(got, "note: v0.6.5") {
		t.Errorf("missing the retry note naming the release that was expected:\n%s", got)
	}
}

// TestUpdateUnreadableInstalledVersionStaysVague keeps the old honest default:
// a version that cannot be read back is never guessed at.
func TestUpdateUnreadableInstalledVersionStaysVague(t *testing.T) {
	stampRelease(t, "v0.6.4")
	stubRunner(t, nil)
	stubInstalledVersion(t, "")

	var out bytes.Buffer
	if err := update(context.Background(), updateTestAppLive(t, "", "v0.6.5"), &out, nil); err != nil {
		t.Fatalf("update: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "install finished") {
		t.Errorf("unreadable version did not fall back to the vague line:\n%s", got)
	}
	if strings.Contains(got, "installed v") {
		t.Errorf("closing line names a version nothing proved:\n%s", got)
	}
}

// TestVersionFromOutput pins the parse against the one-line `hap --version`
// format, and refuses anything that is not a release version.
func TestVersionFromOutput(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"release build", "hap (herd-auto-prompter) v0.6.5\n", "v0.6.5"},
		{"bare semver gets the v prefix", "hap (herd-auto-prompter) 0.6.5\n", "v0.6.5"},
		{"dev build refused", "hap (herd-auto-prompter) dev-20260812042612\n", ""},
		{"unstamped dev refused", "hap (herd-auto-prompter) dev\n", ""},
		{"empty output", "", ""},
		{"garbage refused", "usage: hap <command>", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := versionFromOutput(tc.in); got != tc.want {
				t.Errorf("versionFromOutput(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestReadInstalledVersionRefusesUnderTest is the structural guarantee that no
// unit test ever executes whatever binary the host's herdr registry names.
func TestReadInstalledVersionRefusesUnderTest(t *testing.T) {
	if got := readInstalledVersion(context.Background()); got != "" {
		t.Errorf("readInstalledVersion under `go test` = %q, want refusal", got)
	}
}

// TestUpdateArgsMatchDocs pins the argv against the README's upgrade command.
func TestUpdateArgsMatchDocs(t *testing.T) {
	want := "plugin install " + updatecheck.Repo + " --yes"
	if got := strings.Join(updateArgs(), " "); got != want {
		t.Errorf("update argv = %q, want %q", got, want)
	}
}

// TestHerdrBinHonoursEnv keeps `hap update` on the same herdr the rest of the
// plugin talks to.
func TestHerdrBinHonoursEnv(t *testing.T) {
	t.Setenv("HERDR_BIN_PATH", "/opt/herdr/bin/herdr")
	if got := herdrBin(); got != "/opt/herdr/bin/herdr" {
		t.Errorf("herdrBin() = %q, want the HERDR_BIN_PATH value", got)
	}
	t.Setenv("HERDR_BIN_PATH", "")
	if got := herdrBin(); got != "herdr" {
		t.Errorf("herdrBin() = %q, want the PATH fallback", got)
	}
}
