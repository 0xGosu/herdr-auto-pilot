package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
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
	latest := latestKnownVersion(ctx, app)
	if latest != "" {
		fmt.Fprintf(out, "installing %s (current: %s)\n", latest, buildinfo.Label())
	} else {
		fmt.Fprintf(out, "installing the latest release (current: %s)\n", buildinfo.Label())
	}
	fmt.Fprintf(out, "running: %s %s\n", herdrBin(), strings.Join(updateArgs(), " "))

	if err := updateRunner(ctx, out); err != nil {
		return err
	}
	if latest != "" {
		fmt.Fprintf(out, "\ninstalled %s\n", latest)
	} else {
		fmt.Fprintln(out, "\ninstall finished")
	}
	printEnsureStep(out)
	return nil
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
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// latestKnownVersion resolves the release being installed: the cached check
// first, then one live check. It is advisory — an unknown version only costs a
// vaguer message, never the upgrade.
func latestKnownVersion(ctx context.Context, app *frontend.App) string {
	if app == nil {
		return ""
	}
	if app.StateDir != "" {
		if st, ok := updatecheck.Read(app.StateDir); ok && updatecheck.IsRelease(st.LatestVersion) {
			return st.LatestVersion
		}
	}
	status, err := app.CheckForUpdate(ctx)
	if err != nil {
		return ""
	}
	return status.Latest
}
