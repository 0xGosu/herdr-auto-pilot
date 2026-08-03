package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// stateFootprint is one measured file (or file group) in the state directory.
type stateFootprint struct {
	name  string
	bytes int64
}

// measureStateDir reports what hap is using disk for, largest first. Missing
// files are reported as zero rather than skipped: "the log is 0 bytes" is a
// useful answer, and a stat error here must never fail the command.
func measureStateDir(stateDir string) []stateFootprint {
	if stateDir == "" {
		return nil
	}
	sizeOf := func(names ...string) int64 {
		var total int64
		for _, n := range names {
			if fi, err := os.Stat(filepath.Join(stateDir, n)); err == nil {
				total += fi.Size()
			}
		}
		return total
	}
	out := []stateFootprint{
		{"database", sizeOf("herd-auto-prompter.db", "herd-auto-prompter.db-wal", "herd-auto-prompter.db-shm")},
		{"plugin log", sizeOf("herd-auto-prompter.log", "herd-auto-prompter.log.old")},
		{"daemon stderr log", sizeOf("daemon.stderr.log", "daemon.stderr.log.old")},
		{"match index", dirSize(filepath.Join(stateDir, "match-index"))},
	}
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].bytes > out[i].bytes {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// dirSize sums a directory's regular files, ignoring anything unreadable.
func dirSize(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // an unreadable entry is skipped, never fatal
		}
		if fi, err := d.Info(); err == nil {
			total += fi.Size()
		}
		return nil
	})
	return total
}

// humanDays renders a retention window in the unit it is configured in.
// time.Duration would print 14 days as "336h0m0s".
func humanDays(d time.Duration) string {
	days := int(d.Hours() / 24)
	switch {
	case d == 0:
		// Retention 0: nothing is kept. Rendering this as "0 days" invites the
		// reading "nothing happens", which is the opposite of what it does.
		return "no excerpts kept"
	case days == 1:
		return "1 day"
	case days > 0:
		return fmt.Sprintf("%d days", days)
	}
	return d.String()
}

// blankPhrase describes what a run would do, as a verb phrase. Retention 0 gets
// its own wording because "blank excerpts older than no excerpts kept" is not a
// sentence — the window doubles as a description of the setting elsewhere.
func blankPhrase(window time.Duration) string {
	if window == 0 {
		return "blank EVERY eligible pane excerpt (retention is 0)"
	}
	return "blank pane excerpts older than " + humanDays(window)
}

// humanBytes renders a size the way an operator reads `du -h`.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}

// gc reclaims disk from hap's own accumulating records.
//
// It exists because the daemon's sweep runs once a day and only while a daemon
// is running: an operator who has just noticed the state directory is large
// wants the space back now, and wants to see what it would do first.
func gc(ctx context.Context, app *frontend.App, out io.Writer, args []string) error {
	fs := flag.NewFlagSet("gc", flag.ContinueOnError)
	days := fs.Int("days", daysUnset,
		"blank pane excerpts older than N days; 0 blanks them all (default: the configured retention)")
	dryRun := fs.Bool("dry-run", false, "report what would be reclaimed, change nothing")
	fs.SetOutput(out)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *days < 0 && *days != daysUnset {
		return fmt.Errorf("--days must be 0 or more (0 blanks every eligible excerpt)")
	}

	window, enabled := retentionWindow(app, *days)
	if !enabled {
		return fmt.Errorf("excerpt retention is turned off " +
			"(`[logging] audit_excerpt_retention_days` is negative) — pass --days N to override for this run")
	}

	before := measureStateDir(app.StateDir)
	fmt.Fprintln(out, "Before:")
	printFootprint(out, before)

	rp, ok := app.Store.(ports.RetentionPort)
	if !ok {
		return fmt.Errorf("this store does not support retention")
	}
	now := time.Now()

	if *dryRun {
		// Reported, not performed: the same cutoff the real run would use, so
		// the operator can see the window before committing to it.
		fmt.Fprintf(out, "\nDry run: would %s.\n", blankPhrase(window))
		fmt.Fprintln(out, "Rows still in play are never touched — pending escalations at any age,")
		fmt.Fprintln(out, "rows with an unprocessed LLM retry, and recently answered asks.")
		return nil
	}

	n, err := rp.PruneAuditExcerpts(ctx, now, now.Add(-window))
	if err != nil {
		return err
	}
	if window == 0 {
		fmt.Fprintf(out, "\nCleared pane excerpts on %d audit row(s) — retention is 0, so none are kept.\n", n)
	} else {
		fmt.Fprintf(out, "\nCleared pane excerpts on %d audit row(s) older than %s.\n", n, humanDays(window))
	}

	// Always vacuum here, unlike the daemon's threshold: the operator asked
	// for the space back, so the write lock is the point rather than a cost.
	free, err := rp.FreelistPages(ctx)
	if err != nil {
		return err
	}
	if free > 0 {
		if err := rp.Vacuum(ctx); err != nil {
			return err
		}
	}

	after := measureStateDir(app.StateDir)
	fmt.Fprintln(out, "\nAfter:")
	printFootprint(out, after)
	fmt.Fprintf(out, "\nReclaimed %s.\n", humanBytes(totalBytes(before)-totalBytes(after)))
	return nil
}

// daysUnset is --days's "not given" sentinel. It cannot be 0: `--days 0` is a
// real instruction ("keep no excerpts"), the same as the config value, so 0
// must not double as "fall back to the config".
const daysUnset = -1

// retentionWindow resolves the cutoff: an explicit --days wins, otherwise the
// configured retention (which may be off entirely).
func retentionWindow(app *frontend.App, days int) (time.Duration, bool) {
	if days >= 0 {
		return time.Duration(days) * 24 * time.Hour, true
	}
	cfg, err := config.Load(app.ConfigPath)
	if err != nil {
		cfg = config.Default()
	}
	return cfg.Logging.AuditExcerptRetention()
}

func totalBytes(fp []stateFootprint) int64 {
	var t int64
	for _, f := range fp {
		t += f.bytes
	}
	return t
}

func printFootprint(out io.Writer, fp []stateFootprint) {
	if len(fp) == 0 {
		fmt.Fprintln(out, "  (state directory unknown)")
		return
	}
	for _, f := range fp {
		fmt.Fprintf(out, "  %-20s %10s\n", f.name, humanBytes(f.bytes))
	}
	fmt.Fprintf(out, "  %-20s %10s\n", "total", humanBytes(totalBytes(fp)))
}
