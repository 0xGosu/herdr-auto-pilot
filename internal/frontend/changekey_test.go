package frontend

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
	"github.com/0xGosu/herdr-auto-pilot/internal/tasklocator"
)

// TestChangeKeyMovesWithEverySourceARefreshReads pins what lets the TUI skip a
// re-read: the key holds still across reads and moves on a store write, a
// config edit and a ticked checklist FILE — each a source a refresh reads.
// Every case that moves it is the control for the stability check before it.
func TestChangeKeyMovesWithEverySourceARefreshReads(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	list := filepath.Join(t.TempDir(), "tasks.md")
	if err := os.WriteFile(list, []byte("- [ ] one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	groups := []TaskGroup{
		{Locator: list},
		// A db:// list lives in the store: nothing to stat, never a refusal.
		{Locator: tasklocator.DBLocator(st.NodeID(), "agent.md")},
		// An unresolved group has no locator at all.
		{Err: "one list per matched agent"},
	}
	key := func() string {
		t.Helper()
		k, ok := app.ChangeKey(ctx, groups)
		if !ok {
			t.Fatal("ChangeKey could not tell over a sqlite store and local files")
		}
		return k
	}
	// Each write below lands at a distinct, later mtime: a coarse filesystem
	// clock can otherwise give two writes in one test the same stamp.
	bump := func(path string, at time.Time) {
		t.Helper()
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	base := time.Now().Add(time.Hour)

	k0 := key()
	if _, err := st.AuditLog(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := app.GetStatus(ctx); err != nil {
		t.Fatal(err)
	}
	if got := key(); got != k0 {
		t.Fatalf("reads moved the key %q → %q", k0, got)
	}

	seedEscalationRow(t, st)
	k1 := key()
	if k1 == k0 {
		t.Error("a store write left the key where it was")
	}

	if err := os.WriteFile(app.ConfigPath, []byte("[tui]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bump(app.ConfigPath, base)
	k2 := key()
	if k2 == k1 {
		t.Error("a config edit left the key where it was")
	}

	// Same size on purpose: ticking an item rewrites one byte.
	if err := os.WriteFile(list, []byte("- [x] one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bump(list, base.Add(time.Second))
	if got := key(); got == k2 {
		t.Error("a ticked checklist file left the key where it was")
	}
}

// TestChangeKeyRefusesWhatItCannotSee: a gist list changes with no local trace
// and a store without a change token cannot say anything, so both answer
// false and the TUI keeps re-reading every tick, exactly as before.
func TestChangeKeyRefusesWhatItCannotSee(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	if _, ok := app.ChangeKey(ctx, []TaskGroup{{Locator: tasklocator.GistLocator("abc123", "tasks.md")}}); ok {
		t.Error("a gist list produced a change key")
	}
	bare := &App{Store: struct{ ports.StorePort }{st}, ConfigPath: app.ConfigPath} // hides Revision
	if _, ok := bare.ChangeKey(ctx, nil); ok {
		t.Error("a store without a change token produced a key")
	}
	if _, ok := app.ChangeKey(ctx, nil); !ok {
		t.Error("control: the sqlite store with no groups should produce a key")
	}
}
