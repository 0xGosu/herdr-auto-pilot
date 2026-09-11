package frontend

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
	"github.com/0xGosu/herdr-auto-pilot/internal/tasklocator"
)

// ChangeKey returns a token that moves whenever a TUI refresh could show
// something different, and false when this App cannot tell — the caller then
// refreshes as it always did.
//
// A refresh reads three kinds of source, and the key covers each:
//
//   - the store, through its change token (ports.RevisionReporter) — under
//     turso that is one tiny request to the daemon instead of the few dozen
//     queries a refresh makes, which is what a TUI's idle cost was;
//   - config.toml, by size and modification time;
//   - the checklist FILES among groups — the task groups the last refresh
//     resolved — since an agent ticking off its list changes no store row.
//     A db:// list lives in the store and is already covered.
//
// A gist list changes with no local trace at all, so a config reading one
// answers false: under a gist source the TUI keeps re-reading every tick.
// Anything else a refresh derives from the clock (roster freshness, the update
// check's due time) is the caller's to re-read on a backstop.
func (a *App) ChangeKey(ctx context.Context, groups []TaskGroup) (string, bool) {
	rr, ok := a.Store.(ports.RevisionReporter)
	if !ok {
		return "", false
	}
	rev, err := rr.Revision(ctx)
	if err != nil {
		return "", false
	}
	var b strings.Builder
	b.WriteString(rev)
	if !stampFile(&b, a.ConfigPath) {
		return "", false
	}
	for _, g := range groups {
		switch tasklocator.Scheme(g.Locator) {
		case tasklocator.DBScheme:
		case "":
			if g.Locator != "" && !stampFile(&b, g.Locator) {
				return "", false
			}
		default:
			return "", false
		}
	}
	return b.String(), true
}

// stampFile appends path's size and modification time to b ("-" when it does
// not exist, which is a state too), reporting false when it cannot be read.
func stampFile(b *strings.Builder, path string) bool {
	b.WriteString("|")
	if path == "" {
		return true
	}
	fi, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		b.WriteString("-")
	case err != nil:
		return false
	default:
		fmt.Fprintf(b, "%d:%d", fi.ModTime().UnixNano(), fi.Size())
	}
	return true
}
