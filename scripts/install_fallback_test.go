// Coverage for scripts/install.sh, in particular the release fallback: when
// the version herdr-plugin.toml declares has no downloadable assets, the
// installer substitutes the newest EARLIER release that does.
//
// That window is routine — auto-release bumps the manifest to the next version
// and tags it, then release.yml spends ~15 minutes building on three native
// runners — so the fallback is what keeps `herdr plugin install` and
// `hap update` working off main. It is also unguarded by anything else: there
// is no shellcheck or bats in this repo, and nothing else in CI executes it.
//
// The network is faked with a `curl` shim first on PATH, serving a directory
// tree of synthetic releases. Everything else the script uses (git, tar,
// install, sha256sum) is real.
package scripts_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const modelFile = "all-minilm-l6-v2-q8_0.gguf"

// release describes one synthetic GitHub release in the fake asset store.
type release struct {
	version string // "0.6.1" — no v prefix
	// corrupt makes SHA256SUMS disagree with the binary it ships, which is
	// the shape of a truncated download or a tampered asset.
	corrupt bool
}

// installEnv is a throwaway plugin directory plus the fake release store the
// curl shim serves from.
type installEnv struct {
	root  string // plugin root: holds herdr-plugin.toml, scripts/, bin/, lib/
	store string // <store>/v<version>/<asset>
	path  string // PATH with the curl shim prepended
}

// installScript resolves scripts/install.sh relative to this test file.
func installScript(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("install.sh")
	if err != nil {
		t.Fatalf("resolve install.sh: %v", err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("install.sh missing: %v", err)
	}
	return abs
}

// assetNames returns the platform-specific asset names install.sh will ask
// for. Derived from the Go toolchain rather than hardcoded so the test is
// meaningful on a darwin/arm64 workstation as well as linux CI.
func assetNames(t *testing.T) (binary, native string) {
	t.Helper()
	arch := runtime.GOARCH
	if runtime.GOOS == "darwin" && arch == "amd64" {
		// install.sh refuses Intel macOS outright; there is no release to
		// fall back TO, so there is nothing here to test.
		t.Skip("Intel macOS is unsupported by install.sh by design")
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("install.sh supports linux and darwin only")
	}
	return fmt.Sprintf("hap-%s-%s", runtime.GOOS, arch),
		fmt.Sprintf("hap-native-%s-%s.tar.gz", runtime.GOOS, arch)
}

func requireTools(t *testing.T) {
	t.Helper()
	requireGit(t)
	for _, tool := range []string{"bash", "tar", "install"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	if _, err := exec.LookPath("sha256sum"); err != nil {
		if _, err := exec.LookPath("shasum"); err != nil {
			t.Skip("neither sha256sum nor shasum available")
		}
	}
}

// newInstallEnv builds a plugin root whose manifest declares manifestVersion,
// whose git history carries tags, and whose fake store publishes releases.
func newInstallEnv(t *testing.T, manifestVersion string, tags []string, releases []release) *installEnv {
	t.Helper()
	requireTools(t)

	env := &installEnv{root: t.TempDir(), store: t.TempDir()}

	if err := os.MkdirAll(filepath.Join(env.root, "scripts"), 0o755); err != nil {
		t.Fatalf("mkdir scripts: %v", err)
	}
	script, err := os.ReadFile(installScript(t))
	if err != nil {
		t.Fatalf("read install.sh: %v", err)
	}
	writeFile(t, filepath.Join(env.root, "scripts", "install.sh"), script, 0o755)
	// Only the version line matters to the script's sed, but keep the shape
	// of a real manifest so a future parser change is exercised honestly.
	writeFile(t, filepath.Join(env.root, "herdr-plugin.toml"), []byte(
		"id = \"herd-auto-prompter\"\nversion = \""+manifestVersion+"\"\n"), 0o644)

	initRepo(t, env.root)
	git(t, env.root, "commit", "-q", "--allow-empty", "-m", "base")
	git(t, env.root, "remote", "add", "origin", "https://github.com/example/hap.git")
	for _, tag := range tags {
		git(t, env.root, "tag", tag)
	}

	for _, rel := range releases {
		env.publish(t, rel)
	}
	env.path = env.shimPath(t)
	return env
}

// publish writes one release's assets, with a SHA256SUMS that covers them.
func (e *installEnv) publish(t *testing.T, rel release) {
	t.Helper()
	binary, native := assetNames(t)
	dir := filepath.Join(e.store, "v"+rel.version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir release dir: %v", err)
	}

	// The "binary" is what install.sh execs for the daemon handover, so it
	// has to actually run. Its version string is how a test tells the
	// releases apart.
	bin := []byte("#!/bin/sh\necho hap v" + rel.version + "\nexit 0\n")
	tarball := mustTarGz(t)
	model := []byte("fake embedding model\n")

	writeFile(t, filepath.Join(dir, binary), bin, 0o644)
	writeFile(t, filepath.Join(dir, native), tarball, 0o644)
	writeFile(t, filepath.Join(dir, modelFile), model, 0o644)

	sums := map[string][]byte{binary: bin, native: tarball, modelFile: model}
	if rel.corrupt {
		sums[binary] = []byte("something else entirely")
	}
	// GNU two-space format with flat basenames — exactly what release.yml's
	// `cd dist && sha256sum *` emits and what install.sh's
	// `grep " $asset$"` matches against. Computed here rather than by
	// exec'ing sha256sum, which macOS does not ship.
	var out bytes.Buffer
	for _, name := range []string{binary, native, modelFile} {
		fmt.Fprintf(&out, "%x  %s\n", sha256.Sum256(sums[name]), name)
	}
	writeFile(t, filepath.Join(dir, "SHA256SUMS"), out.Bytes(), 0o644)
}

// unpublish removes a release's assets while leaving its tag in place — the
// shape of a tagged version whose release build has not finished (or failed).
func (e *installEnv) unpublish(t *testing.T, version string) {
	t.Helper()
	if err := os.RemoveAll(filepath.Join(e.store, "v"+version)); err != nil {
		t.Fatalf("unpublish v%s: %v", version, err)
	}
}

// mustTarGz builds a real lib/ tarball, since install.sh untars it for real.
func mustTarGz(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("not really a shared library\n")
	hdr := &tar.Header{Name: "lib/libfake.so", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatalf("tar body: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// shimPath writes a `curl` that serves the fake store and returns a PATH with
// it in front. Because the shim IS curl, install.sh's --retry-delay never
// sleeps and the failure paths run instantly.
func (e *installEnv) shimPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	shim := `#!/bin/sh
# Fake curl. Understands only what install.sh actually passes: -o <file>, a
# HEAD request (-I, which arrives bundled as -fsI), and the URL. Every other
# flag is IGNORED rather than rejected, so adding one to install.sh does not
# fail the test for the wrong reason.
STORE='` + e.store + `'
out=''
head=0
url=''
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    --max-time|--retry|--retry-delay) shift 2 ;;
    --*) shift ;;
    -*) case "$1" in *I*) head=1 ;; esac; shift ;;
    *) url="$1"; shift ;;
  esac
done
tag=$(echo "$url" | sed -n 's#.*/download/\([^/]*\)/.*#\1#p')
file=$(echo "$url" | sed -n 's#.*/download/[^/]*/\(.*\)$#\1#p')
src="$STORE/$tag/$file"
if [ ! -f "$src" ]; then
  echo "curl: (22) The requested URL returned error: 404" >&2
  exit 22
fi
if [ "$head" = 1 ]; then
  exit 0
fi
if [ -n "$out" ]; then
  cat "$src" > "$out"
else
  cat "$src"
fi
exit 0
`
	writeFile(t, filepath.Join(dir, "curl"), []byte(shim), 0o755)

	// A fake `mv` so a test can make ONE move of the swap fail. A full or
	// read-only disk is the real cause and cannot be simulated as root, which
	// bypasses permission checks; injecting the failure at the call is the only
	// way to reach the rollback path at all.
	realMv, err := exec.LookPath("mv")
	if err != nil {
		t.Skip("mv not available")
	}
	mvShim := `#!/bin/sh
# Fails when the SOURCE matches the $HAP_TEST_MV_FAIL glob, else defers to the
# real mv. Matching the source rather than the destination is deliberate: it
# lets a test break one move of the swap while leaving the rollback's own moves
# — whose sources live under .hap-prev — working.
if [ -n "${HAP_TEST_MV_FAIL:-}" ]; then
  case "$1" in
    $HAP_TEST_MV_FAIL) echo "mv: simulated failure: $1" >&2; exit 1 ;;
  esac
fi
exec '` + realMv + `' "$@"
`
	writeFile(t, filepath.Join(dir, "mv"), []byte(mvShim), 0o755)
	return dir + string(os.PathListSeparator) + os.Getenv("PATH")
}

// run executes install.sh against this environment. extraEnv entries are
// appended last, so they win.
func (e *installEnv) run(t *testing.T, extraEnv ...string) (int, string) {
	t.Helper()
	cmd := exec.Command("bash", filepath.Join(e.root, "scripts", "install.sh"))
	cmd.Dir = e.root
	// gitEnv seals git the way the sibling guard test does — an ambient
	// GIT_DIR would otherwise point the tag lookup at the developer's own
	// repository. HAP_* are stripped for the same reason: an operator with
	// HAP_VERSION exported would silently invert half these cases.
	env := make([]string, 0, len(os.Environ())+4)
	for _, kv := range gitEnv() {
		if strings.HasPrefix(kv, "PATH=") || strings.HasPrefix(kv, "HAP_") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "PATH="+e.path)
	cmd.Env = append(env, extraEnv...)

	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("run install.sh: %v\n%s", err, out)
	}
	return exit.ExitCode(), string(out)
}

// installedVersion reports which release bin/hap came from, by running it.
func (e *installEnv) installedVersion(t *testing.T) string {
	t.Helper()
	out, err := exec.Command(filepath.Join(e.root, "bin", "hap")).Output()
	if err != nil {
		t.Fatalf("run installed binary: %v", err)
	}
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(out)), "hap v"))
}

func (e *installEnv) exists(path string) bool {
	_, err := os.Stat(filepath.Join(e.root, path))
	return err == nil
}

// seedPriorInstall puts a working older install in place. Asserting that a
// FAILED run "left nothing behind" is vacuous against an empty directory — the
// regression worth guarding is a run that destroys the install the operator
// already had, which only a pre-existing one can detect.
func (e *installEnv) seedPriorInstall(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(e.root, "bin"), 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(e.root, "lib"), 0o755); err != nil {
		t.Fatalf("mkdir lib: %v", err)
	}
	writeFile(t, filepath.Join(e.root, "bin", "hap"), []byte("#!/bin/sh\necho hap v0.0.1\nexit 0\n"), 0o755)
	writeFile(t, filepath.Join(e.root, "lib", "libsentinel.so"), []byte("previous install\n"), 0o644)
}

// assertPriorInstallIntact fails unless the seeded install survived byte for
// byte — neither replaced nor deleted.
func (e *installEnv) assertPriorInstallIntact(t *testing.T, out string) {
	t.Helper()
	if got := e.installedVersion(t); got != "0.0.1" {
		t.Errorf("the previous install was replaced (now v%s) by a run that failed:\n%s", got, out)
	}
	body, err := os.ReadFile(filepath.Join(e.root, "lib", "libsentinel.so"))
	if err != nil {
		t.Errorf("a failed run deleted the previous lib/: %v\n%s", err, out)
		return
	}
	if string(body) != "previous install\n" {
		t.Errorf("a failed run overwrote the previous lib/:\n%s", out)
	}
}

func writeFile(t *testing.T, path string, content []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, content, mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustNotContain(t *testing.T, out string, unwanted ...string) {
	t.Helper()
	for _, u := range unwanted {
		if strings.Contains(out, u) {
			t.Errorf("output should not contain %q:\n%s", u, out)
		}
	}
}

// The window this feature exists for: the manifest already names 0.6.2 while
// release.yml is still building it, so nothing can be downloaded for that
// version and the newest earlier release has to stand in.
func TestInstallFallsBackToTheNewestEarlierRelease(t *testing.T) {
	env := newInstallEnv(t, "0.6.2",
		[]string{"v0.5.9", "v0.6.0", "v0.6.1", "v0.6.2"},
		[]release{{version: "0.5.9"}, {version: "0.6.0"}, {version: "0.6.1"}})

	code, out := env.run(t)
	if code != 0 {
		t.Fatalf("install should have succeeded via fallback (exit %d):\n%s", code, out)
	}
	if got := env.installedVersion(t); got != "0.6.1" {
		t.Errorf("installed v%s, want the newest earlier release v0.6.1:\n%s", got, out)
	}
	if !env.exists("lib/libfake.so") {
		t.Errorf("native libraries were not unpacked:\n%s", out)
	}
	// The operator must be able to see, in the install output, that they did
	// not get the version the plugin declares.
	mustContain(t, out, "installed hap v0.6.1", "NOT the v0.6.2")
}

// Ordering is the whole point of the walk, and it is easy to get backwards: a
// plain `sort -r` is IGNORED by keys carrying their own ordering flags, which
// silently yields the OLDEST release instead of the newest. 0.10.0 beats 0.9.9
// numerically but loses a string sort, so it is the version that tells the two
// apart.
func TestInstallFallbackPrefersTheHighestVersionNotTheFirstTag(t *testing.T) {
	env := newInstallEnv(t, "0.10.1",
		[]string{"v0.1.0", "v0.2.0", "v0.9.9", "v0.10.0"},
		[]release{{version: "0.1.0"}, {version: "0.2.0"}, {version: "0.9.9"}, {version: "0.10.0"}})

	code, out := env.run(t)
	if code != 0 {
		t.Fatalf("install failed (exit %d):\n%s", code, out)
	}
	if got := env.installedVersion(t); got != "0.10.0" {
		t.Errorf("installed v%s, want v0.10.0:\n%s", got, out)
	}
}

// Which candidates count as "earlier" is a separate decision from how they are
// ordered, and it has its own trap: packing the components into one number
// picks a radix, and a component that outgrows it misranks the comparison.
// Under a 1000-radix 0.9.1001 scores ABOVE 0.10.0, so it is dropped from the
// candidates entirely and the walk silently skips to a much older release.
func TestInstallFallbackKeepsCandidatesAPackedComparisonWouldDrop(t *testing.T) {
	env := newInstallEnv(t, "0.10.0",
		[]string{"v0.2.0", "v0.9.1001", "v0.10.0"},
		[]release{{version: "0.2.0"}, {version: "0.9.1001"}})

	code, out := env.run(t)
	if code != 0 {
		t.Fatalf("install failed (exit %d):\n%s", code, out)
	}
	if got := env.installedVersion(t); got != "0.9.1001" {
		t.Errorf("installed v%s, want v0.9.1001 — it IS below v0.10.0:\n%s", got, out)
	}
}

// A tag exists the moment auto-release pushes it; its assets appear ~15
// minutes later, or never if the build fails. The walk must step over such a
// tag rather than stopping at it.
func TestInstallFallbackWalksPastATaggedButUnpublishedRelease(t *testing.T) {
	env := newInstallEnv(t, "0.6.2",
		[]string{"v0.6.0", "v0.6.1", "v0.6.2"},
		[]release{{version: "0.6.0"}, {version: "0.6.1"}})
	env.unpublish(t, "0.6.1")

	code, out := env.run(t)
	if code != 0 {
		t.Fatalf("install failed (exit %d):\n%s", code, out)
	}
	if got := env.installedVersion(t); got != "0.6.0" {
		t.Errorf("installed v%s, want v0.6.0 (v0.6.1 is tagged but unpublished):\n%s", got, out)
	}
}

// HAP_VERSION names an exact release. Substituting a different one would
// defeat the only mechanism an operator has for pinning.
func TestInstallPinnedVersionNeverFallsBack(t *testing.T) {
	// The manifest declares a version that IS published, and the pin names a
	// different one that is not — so the pin has to be what drives the
	// request, not just what blocks the fallback.
	env := newInstallEnv(t, "0.6.0",
		[]string{"v0.6.0", "v0.6.1"},
		[]release{{version: "0.6.0"}})

	code, out := env.run(t, "HAP_VERSION=0.6.1")
	if code != 1 {
		t.Fatalf("a pinned version should fail rather than downgrade (exit %d):\n%s", code, out)
	}
	if env.exists("bin/hap") {
		t.Errorf("a pinned failure installed a binary anyway:\n%s", out)
	}
	mustContain(t, out, "pinned version v0.6.1")
	mustNotContain(t, out, "installed hap-")
}

// Set-but-empty is not a pin. `HAP_VERSION=` falls through to the manifest
// version, so treating it as a pin would refuse the fallback in the name of a
// choice the operator never made — and say so in the error, which is worse
// than useless. (`${HAP_VERSION+set}` fires on empty; `${HAP_VERSION:-}` does
// not.)
func TestInstallEmptyHapVersionIsNotAPin(t *testing.T) {
	env := newInstallEnv(t, "0.6.2",
		[]string{"v0.6.1", "v0.6.2"},
		[]release{{version: "0.6.1"}})

	code, out := env.run(t, "HAP_VERSION=")
	if code != 0 {
		t.Fatalf("an empty HAP_VERSION should not block the fallback (exit %d):\n%s", code, out)
	}
	if got := env.installedVersion(t); got != "0.6.1" {
		t.Errorf("installed v%s, want v0.6.1:\n%s", got, out)
	}
	mustNotContain(t, out, "pinned version")
}

// The binary and lib/ have to land together — the binary is rpath-linked
// against those libraries, so a new binary beside old or partial libraries does
// not start. Whichever half of the swap fails, BOTH halves must go back.
//
// This is the failure the staging directory exists for. Before it, the swap was
// a cross-filesystem `mv` from /tmp, which degrades to copy-then-delete and can
// fail halfway with the old install already gone.
func TestInstallSwapFailureRestoresThePreviousInstall(t *testing.T) {
	for _, tc := range []struct{ name, failSource string }{
		{"library move fails", "*/.hap-install/lib"},
		{"binary move fails", "*/.hap-install/hap"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newInstallEnv(t, "0.6.1",
				[]string{"v0.6.1"}, []release{{version: "0.6.1"}})
			env.seedPriorInstall(t)

			code, out := env.run(t, "HAP_TEST_MV_FAIL="+tc.failSource)
			if code != 1 {
				t.Fatalf("a failed swap must fail the install (exit %d):\n%s", code, out)
			}
			env.assertPriorInstallIntact(t, out)
			// The new payload must be gone too — a leftover half is what makes
			// the pairing invalid.
			if env.exists("lib/libfake.so") {
				t.Errorf("the new libraries survived a rolled-back swap:\n%s", out)
			}
			mustContain(t, out, "restoring the previous install")
		})
	}
}

// The failure BETWEEN the two saves: lib/ has already been moved aside when
// saving the binary fails. Returning early there would leave the old binary
// live with its libraries stranded in the holding directory — a working
// install turned unusable by a failed upgrade. Every failure after the first
// live move has to reach the rollback.
func TestInstallSwapFailureWhileSavingTheBinaryStillRestoresLib(t *testing.T) {
	env := newInstallEnv(t, "0.6.1",
		[]string{"v0.6.1"}, []release{{version: "0.6.1"}})
	env.seedPriorInstall(t)

	// Only the `mv bin/hap .hap-swap/hap` step has this exact source.
	code, out := env.run(t, "HAP_TEST_MV_FAIL=bin/hap")
	if code != 1 {
		t.Fatalf("a failed swap must fail the install (exit %d):\n%s", code, out)
	}
	env.assertPriorInstallIntact(t, out)
	if env.exists(".hap-swap/lib") || env.exists(".hap-prev/lib") {
		t.Errorf("the previous lib/ was left stranded in a holding directory:\n%s", out)
	}
}

// A retry after a failed rollback must not destroy the recovery copy before it
// has succeeded. Clearing it up front means a second failure loses the last
// complete install on the disk.
func TestInstallRetryAfterFailedRollbackKeepsTheRecoveryCopy(t *testing.T) {
	env := newInstallEnv(t, "0.6.1",
		[]string{"v0.6.1"}, []release{{version: "0.6.1"}})
	env.seedPriorInstall(t)

	// Run 1: swap fails and the restore of lib/ fails too, so the copy is kept.
	if code, out := env.run(t, "HAP_TEST_MV_FAIL=*/.hap-*/lib"); code != 1 {
		t.Fatalf("run 1 should have failed (exit %d):\n%s", code, out)
	}
	if !env.exists(".hap-prev/lib/libsentinel.so") {
		t.Fatalf("run 1 did not retain a recovery copy")
	}

	// Run 2: fails early, before anything is placed. The recovery copy from
	// run 1 must still be there afterwards.
	code, out := env.run(t, "HAP_TEST_MV_FAIL=bin/hap")
	if code != 1 {
		t.Fatalf("run 2 should have failed (exit %d):\n%s", code, out)
	}
	body, err := os.ReadFile(filepath.Join(env.root, ".hap-prev", "lib", "libsentinel.so"))
	if err != nil {
		t.Fatalf("a failed retry destroyed the recovery copy: %v\n%s", err, out)
	}
	if string(body) != "previous install\n" {
		t.Errorf("the recovery copy was corrupted by the retry:\n%s", out)
	}

	// Run 3 succeeds, which is what legitimately clears the recovery copy.
	if code, out := env.run(t); code != 0 {
		t.Fatalf("run 3 should have succeeded (exit %d):\n%s", code, out)
	}
	if env.exists(".hap-prev") || env.exists(".hap-swap") {
		t.Errorf("a successful install left holding directories behind")
	}
	if got := env.installedVersion(t); got != "0.6.1" {
		t.Errorf("installed v%s, want v0.6.1", got)
	}
}

// Two failures in sequence: a failed rollback has retained .hap-prev, and THEN
// a later run is killed mid-swap with lib/ already moved into the holding
// directory. That leftover is the only lib/ matching the still-live binary —
// .hap-prev holds an older, different pair — so the next run must recover it,
// not discard it because .hap-prev happens to be occupied.
func TestInstallInterruptedSwapIsRecoveredNotDiscarded(t *testing.T) {
	env := newInstallEnv(t, "0.6.1",
		[]string{"v0.6.1"}, []release{{version: "0.6.1"}})
	env.seedPriorInstall(t)

	// Failure 1: the swap fails and restoring lib/ fails too, so the copy is
	// retained under .hap-prev and lib/ is left absent.
	if code, out := env.run(t, "HAP_TEST_MV_FAIL=*/.hap-*/lib"); code != 1 {
		t.Fatalf("run 1 should have failed (exit %d):\n%s", code, out)
	}
	if !env.exists(".hap-prev/lib") {
		t.Fatalf("run 1 did not retain a recovery copy")
	}

	// Failure 2: a run killed after moving lib/ aside but before the binary.
	// Reconstructed directly — a real SIGKILL is not reproducible in a test.
	// This lib/ is the mate of the binary that is still live.
	if err := os.MkdirAll(filepath.Join(env.root, ".hap-swap", "lib"), 0o755); err != nil {
		t.Fatalf("stage interrupted swap: %v", err)
	}
	writeFile(t, filepath.Join(env.root, ".hap-swap", "lib", "libinterrupted.so"),
		[]byte("mate of the live binary\n"), 0o644)

	// The next run fails early, so nothing it downloads replaces the debris.
	code, out := env.run(t, "HAP_TEST_MV_FAIL=bin/hap")
	if code != 1 {
		t.Fatalf("run 3 should have failed (exit %d):\n%s", code, out)
	}
	body, err := os.ReadFile(filepath.Join(env.root, "lib", "libinterrupted.so"))
	if err != nil {
		t.Fatalf("the interrupted swap's lib/ was discarded instead of recovered: %v\n%s", err, out)
	}
	if string(body) != "mate of the live binary\n" {
		t.Errorf("the recovered lib/ was corrupted:\n%s", out)
	}
	// The older pair is untouched — recovering one must not consume the other.
	if !env.exists(".hap-prev/lib/libsentinel.so") {
		t.Errorf("recovering the interrupted swap destroyed the older recovery copy:\n%s", out)
	}
}

// The last-resort path: the swap fails AND putting the old files back fails
// too. The saved copy is then the only surviving working install, so it must
// NOT be deleted on the way out — the operator is told where it is.
func TestInstallFailedRollbackKeepsTheSavedCopy(t *testing.T) {
	env := newInstallEnv(t, "0.6.1",
		[]string{"v0.6.1"}, []release{{version: "0.6.1"}})
	env.seedPriorInstall(t)

	// Matches the staged lib (triggering the rollback) AND the rollback's own
	// restore of the saved lib — but not `mv lib .hap-prev/lib`, whose source
	// is the bare "lib".
	code, out := env.run(t, "HAP_TEST_MV_FAIL=*/.hap-*/lib")
	if code != 1 {
		t.Fatalf("a failed swap must fail the install (exit %d):\n%s", code, out)
	}
	mustContain(t, out, "could not be fully restored", ".hap-prev")

	// assertPriorInstallIntact does not apply here: lib/ is legitimately gone,
	// which is precisely why the saved copy must still exist.
	body, err := os.ReadFile(filepath.Join(env.root, ".hap-prev", "lib", "libsentinel.so"))
	if err != nil {
		t.Fatalf("the saved copy of the previous install was deleted: %v\n%s", err, out)
	}
	if string(body) != "previous install\n" {
		t.Errorf("the saved copy was corrupted:\n%s", out)
	}
}

// The escape hatch for reproducible installs: fail loudly instead of quietly
// getting something older.
func TestInstallNoFallbackEnvRefusesToSubstitute(t *testing.T) {
	env := newInstallEnv(t, "0.6.2",
		[]string{"v0.6.1", "v0.6.2"},
		[]release{{version: "0.6.1"}})

	code, out := env.run(t, "HAP_NO_FALLBACK=1")
	if code != 1 {
		t.Fatalf("HAP_NO_FALLBACK=1 should fail (exit %d):\n%s", code, out)
	}
	if env.exists("bin/hap") {
		t.Errorf("HAP_NO_FALLBACK=1 installed a binary anyway:\n%s", out)
	}
	mustContain(t, out, "HAP_NO_FALLBACK is set")
}

// Set-but-empty means UNSET, for the same reason `HAP_VERSION=` is not a pin
// and the way every other flag env var in this repo is read. `HAP_NO_FALLBACK=
// bash scripts/install.sh` is how a wrapper CLEARS an inherited flag, so
// honoring it as "set" would enable the opt-out for someone switching it off.
func TestInstallEmptyNoFallbackDoesNotBlockTheFallback(t *testing.T) {
	env := newInstallEnv(t, "0.6.2",
		[]string{"v0.6.1", "v0.6.2"},
		[]release{{version: "0.6.1"}})

	code, out := env.run(t, "HAP_NO_FALLBACK=")
	if code != 0 {
		t.Fatalf("an empty HAP_NO_FALLBACK should not block the fallback (exit %d):\n%s", code, out)
	}
	if got := env.installedVersion(t); got != "0.6.1" {
		t.Errorf("installed v%s, want v0.6.1:\n%s", got, out)
	}
}

// A checksum mismatch is corruption or tampering, never a publishing gap.
// Answering it with a downgrade would turn a security signal into a silent
// version change, so it fails hard and the walk never starts.
func TestInstallChecksumMismatchFailsWithoutFallingBack(t *testing.T) {
	env := newInstallEnv(t, "0.6.2",
		[]string{"v0.6.1", "v0.6.2"},
		[]release{{version: "0.6.1"}, {version: "0.6.2", corrupt: true}})

	env.seedPriorInstall(t)

	code, out := env.run(t)
	if code != 1 {
		t.Fatalf("a checksum mismatch must fail (exit %d):\n%s", code, out)
	}
	mustContain(t, out, "checksum verification failed")
	mustNotContain(t, out, "looking for an earlier release", "installed hap v0.6.1")
	// Verification runs before ANY mutation, so a bad asset never reaches the
	// plugin directory. This is the assertion that catches a reordering of the
	// fetch/verify/install sequence; the messages above only catch a reworded
	// one.
	env.assertPriorInstallIntact(t, out)
}

// When everything the manifest asks for is published, nothing about the old
// behavior changes: no probing, no warning.
func TestInstallPublishedVersionInstallsItselfWithNoWarning(t *testing.T) {
	env := newInstallEnv(t, "0.6.2",
		[]string{"v0.6.1", "v0.6.2"},
		[]release{{version: "0.6.1"}, {version: "0.6.2"}})

	code, out := env.run(t)
	if code != 0 {
		t.Fatalf("install failed (exit %d):\n%s", code, out)
	}
	if got := env.installedVersion(t); got != "0.6.2" {
		t.Errorf("installed v%s, want the declared v0.6.2:\n%s", got, out)
	}
	mustNotContain(t, out, "looking for an earlier release", "NOT the v")
}

// Nothing to fall back TO: no earlier release is published. The install must
// fail the way it always did, and — the part that matters to somebody
// upgrading — must leave the install they already had exactly as it was.
func TestInstallExhaustedFallbackFailsAndLeavesThePreviousInstallIntact(t *testing.T) {
	env := newInstallEnv(t, "0.6.2", []string{"v0.6.1", "v0.6.2"}, nil)
	env.seedPriorInstall(t)

	code, out := env.run(t)
	if code != 1 {
		t.Fatalf("install should have failed (exit %d):\n%s", code, out)
	}
	env.assertPriorInstallIntact(t, out)
	mustContain(t, out, "no earlier release could be installed")
}

// A half-finished release — binary published, native tarball not — must not
// install the binary and delete lib/ before discovering the gap. It falls back
// as a whole, since a release is only usable if every required asset is there.
func TestInstallFallsBackWhenOnlySomeAssetsArePublished(t *testing.T) {
	env := newInstallEnv(t, "0.6.2",
		[]string{"v0.6.1", "v0.6.2"},
		[]release{{version: "0.6.1"}, {version: "0.6.2"}})
	_, native := assetNames(t)
	if err := os.Remove(filepath.Join(env.store, "v0.6.2", native)); err != nil {
		t.Fatalf("remove native asset: %v", err)
	}
	env.seedPriorInstall(t)

	code, out := env.run(t)
	if code != 0 {
		t.Fatalf("install failed (exit %d):\n%s", code, out)
	}
	if got := env.installedVersion(t); got != "0.6.1" {
		t.Errorf("installed v%s, want v0.6.1:\n%s", got, out)
	}
	// The aborted v0.6.2 attempt must not have left its own binary or its own
	// lib/ behind — the fallback replaced BOTH, so no piece of the seeded
	// install and no piece of the partial one survives.
	if env.exists("lib/libsentinel.so") {
		t.Errorf("the previous lib/ survived a successful reinstall:\n%s", out)
	}
	if !env.exists("lib/libfake.so") {
		t.Errorf("v0.6.1's libraries were not unpacked:\n%s", out)
	}
}

// A clone with no local tags — herdr may clone shallow — still has to find
// candidates, which is what the `git ls-remote` branch is for. Pointing origin
// at a local bare repo exercises it without a network.
func TestInstallFallbackFindsCandidatesViaLsRemote(t *testing.T) {
	env := newInstallEnv(t, "0.6.2",
		[]string{"v0.6.0", "v0.6.1"},
		[]release{{version: "0.6.0"}, {version: "0.6.1"}})

	bare := filepath.Join(t.TempDir(), "origin.git")
	git(t, env.root, "clone", "-q", "--bare", env.root, bare)
	git(t, env.root, "remote", "set-url", "origin", "file://"+bare)
	// Force the remote branch: with these gone, `git tag --list` is empty.
	git(t, env.root, "tag", "-d", "v0.6.0", "v0.6.1")
	if tags := git(t, env.root, "tag", "--list", "v*"); strings.TrimSpace(tags) != "" {
		t.Fatalf("local tags should be gone, got %q", tags)
	}

	code, out := env.run(t)
	if code != 0 {
		t.Fatalf("install failed (exit %d):\n%s", code, out)
	}
	if got := env.installedVersion(t); got != "0.6.1" {
		t.Errorf("installed v%s, want v0.6.1 discovered via ls-remote:\n%s", got, out)
	}
}
