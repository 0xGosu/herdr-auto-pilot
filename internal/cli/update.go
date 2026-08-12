package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/buildinfo"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/selfpath"
	"github.com/0xGosu/herdr-auto-pilot/internal/updatecheck"
)

// updateTimeout bounds the reinstall. It downloads a binary, native libs, and
// (optionally) the embedding model, so it is generous compared with the
// 15s herdr CLI reads elsewhere.
const updateTimeout = 15 * time.Minute

// updateRunner performs the reinstall. It is a var so tests substitute a fake
// and never invoke a real herdr.
var updateRunner = runHerdrInstall

// installedBinary and lookPath resolve which binary the operator should run
// after the install. Vars so tests can describe an install layout without one.
var (
	installedBinary = selfpath.Installed
	lookPath        = exec.LookPath
)

// herdrBin resolves the herdr binary the same way internal/herdr does:
// HERDR_BIN_PATH, else "herdr" on PATH.
func herdrBin() string {
	if bin := os.Getenv("HERDR_BIN_PATH"); bin != "" {
		return bin
	}
	return "herdr"
}

// updateArgs is the documented upgrade command (README "Update to the latest
// version"): re-running install fetches and installs the newest release.
func updateArgs() []string {
	return []string{"plugin", "install", updatecheck.Repo, "--yes"}
}

// runHerdrInstall streams the reinstall's output to the operator, so a slow
// download looks like progress rather than a hang.
func runHerdrInstall(ctx context.Context, out io.Writer) error {
	// A test must never reinstall the operator's plugin. The command registry
	// is swept by tests that invoke every handler (see hints_test.go), and
	// `--yes` suppresses every confirmation herdr would otherwise ask for, so
	// this refusal is the structural guarantee — not the sweep's exclusion
	// list, which the next registry test could forget.
	if testing.Testing() {
		return errors.New("refusing to install from a test")
	}
	ctx, cancel := context.WithTimeout(ctx, updateTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, herdrBin(), updateArgs()...)
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("herdr plugin install: %w", err)
	}
	return nil
}

// update implements `hap update`: install the newest published release, then
// tell the operator how to hand the running daemon over to it.
func update(ctx context.Context, app *frontend.App, out io.Writer, args []string) error {
	// A non-release binary is a `herdr plugin link` working tree, so installing
	// over it REPLACES the developer's live checkout with a published release.
	// That is the same build the header hint stays silent about; here it needs
	// an explicit --force rather than a silent clobber.
	if !updatecheck.IsRelease(buildinfo.Version) && !hasFlag(args, "--force") {
		return fmt.Errorf("this build is %s, not a release — installing would replace a linked working tree; rerun as: hap update --force",
			buildinfo.Label())
	}
	// Name the target up front: the install output is herdr's, not ours, so
	// without this the operator cannot tell what version they are getting.
	// "newest known" is a hedge, not sloppiness — the install reads main's
	// manifest while this line reads GitHub's releases, and for the ~15 minutes
	// between a bump merge and its release publishing the two disagree.
	latest := latestKnownVersion(ctx, app)
	if latest != "" {
		fmt.Fprintf(out, "installing the latest release (newest known: %s, current: %s)\n", latest, buildinfo.Label())
	} else {
		fmt.Fprintf(out, "installing the latest release (current: %s)\n", buildinfo.Label())
	}
	fmt.Fprintf(out, "running: %s %s\n", herdrBin(), strings.Join(updateArgs(), " "))

	if err := updateRunner(ctx, out); err != nil {
		return err
	}
	// Report what actually landed, not what was expected: install.sh falls
	// back to the newest release WITH assets when the manifest's version has
	// not published yet, so the installed binary is the only honest source.
	installed := installedVersion(ctx)
	if installed != "" {
		fmt.Fprintf(out, "\ninstalled %s\n", installed)
		if updatecheck.IsNewer(installed, latest) {
			fmt.Fprintf(out, "note: %s exists but its assets were not available yet — run `hap update` again in a few minutes to get it\n", latest)
		}
	} else {
		fmt.Fprintln(out, "\ninstall finished")
	}
	printEnsureStep(out)
	return nil
}

// installedVersion reads the version back from the binary the install left
// behind. A var so tests substitute a fake instead of executing anything.
var installedVersion = readInstalledVersion

// readInstalledVersion runs the newly installed binary's `--version` and
// parses the release out of it. Every failure returns "" — the caller then
// says "install finished" rather than naming a version it cannot prove.
func readInstalledVersion(ctx context.Context) string {
	// The same structural guarantee as runHerdrInstall: no unit test — in any
	// package — ever executes whatever binary the host's herdr registry names.
	if testing.Testing() {
		return ""
	}
	bin := installedBinary()
	if bin == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	raw, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		return ""
	}
	return versionFromOutput(string(raw))
}

// versionFromOutput extracts the release from `hap --version` output
// (buildinfo.VersionLine — the round-trip test pins the two together). Only a
// dotted release version is reported: a dev build's stamp would make
// "installed dev-…" read as a malfunction, and IsRelease alone also accepts a
// bare integer, which any exit-0 diagnostic could legitimately end with.
func versionFromOutput(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	v := fields[len(fields)-1]
	if !strings.Contains(v, ".") || !updatecheck.IsRelease(v) {
		return ""
	}
	return buildinfo.LabelOf(v)
}

// printEnsureStep names the binary that must run `daemon --ensure`.
//
// This process is the OLD build, and the operator's `hap` shortcut is a
// symlink herdr does not repoint on install — so a bare `hap daemon --ensure`
// can hand the daemon to the very binary the upgrade just replaced (or to one
// that no longer exists). When the newly installed binary is discoverable and
// is NOT what `hap` resolves to, the absolute path is printed instead.
func printEnsureStep(out io.Writer) {
	newBin := installedBinary()
	if newBin == "" || sameAsPathHap(newBin) {
		fmt.Fprintln(out, "run `hap daemon --ensure` to hand the running daemon over to the new build now")
		return
	}
	fmt.Fprintf(out, "run `%s daemon --ensure` to hand the running daemon over to the new build now\n", newBin)
	fmt.Fprintln(out, "(the full path is deliberate: `hap` on your PATH still points at the previous build)")
}

// sameAsPathHap reports whether `hap` on PATH already resolves to bin, in
// which case the short command is safe to print.
func sameAsPathHap(bin string) bool {
	p, err := lookPath("hap")
	if err != nil {
		return false
	}
	return selfpath.Same(p, bin)
}

// hasFlag reports whether args carry the given flag.
func hasFlag(args []string, flag string) bool {
	return slices.Contains(args, flag)
}

// latestKnownVersion resolves the newest release GitHub knows of: one live
// check first, falling back to the cached record only when the fetch fails.
// The operator asked to update NOW and the install is about to spend minutes
// downloading, so the 5s fetch is free — while a cached answer can be hours
// stale and name the release BEFORE the one being installed. It is advisory —
// an unknown version only costs a vaguer message, never the upgrade. (Without
// a state dir CheckForUpdate is a no-op rather than a fetch — there is nowhere
// to cache into — so a stateless app degrades to the vague message.)
func latestKnownVersion(ctx context.Context, app *frontend.App) string {
	if app == nil {
		return ""
	}
	if status, err := app.CheckForUpdate(ctx); err == nil && updatecheck.IsRelease(status.Latest) {
		return status.Latest
	}
	if app.StateDir != "" {
		if st, ok := updatecheck.Read(app.StateDir); ok && updatecheck.IsRelease(st.LatestVersion) {
			return st.LatestVersion
		}
	}
	return ""
}
