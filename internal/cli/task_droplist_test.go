package cli

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

const dropListAgent = "otter"

var dropListNow = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

// dropListApp builds an App on the DEFAULT (sqlite) provider with one derived
// source for agent "w1:p1" and its list already seeded in the database.
func dropListApp(t *testing.T) (*frontend.App, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h := &sendRecorderHerdr{agents: []domain.AgentTransition{
		{AgentID: "w1:p1", PaneID: "w1:p1", AgentType: "claude", Status: "idle"},
	}}
	app := &frontend.App{Store: st, Herdr: h, StateDir: dir,
		ConfigPath: filepath.Join(dir, "config.toml"), Author: "operator"}
	seedRoster(t, st, h.agents...)
	// The derived list name comes from the agent's SHORT NAME, not its id: a
	// pane id carries a colon, which is not a legal store file name.
	ctx := context.Background()
	if _, err := st.EnsureAgentName(ctx, "w1:p1"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AdoptAgentName(ctx, "w1:p1", dropListAgent); err != nil {
		t.Fatal(err)
	}
	if err := app.AddTaskSource(ctx, "w1:p1", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsureTaskList(ctx, st.NodeID(), dropListAgent+".md", dropListAgent,
		"# Tasks\n\n- [ ] alpha\n- [ ] beta\n", dropListNow); err != nil {
		t.Fatal(err)
	}
	return app, st
}

func runDrop(t *testing.T, app *frontend.App, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := Run(context.Background(), app, &out, "task", args)
	return out.String(), err
}

// listGone reports whether the seeded list is no longer in the database.
func listGone(t *testing.T, st *store.Store) bool {
	t.Helper()
	_, err := st.ReadTaskList(context.Background(), st.NodeID(), dropListAgent+".md")
	return errors.Is(err, fs.ErrNotExist)
}

// TestTaskDropListConfirmation: N aborts and changes nothing, y deletes, and a
// scripted run with no terminal refuses rather than silently no-oping.
//
// The prompt must name the item COUNT and the recreation, for the same reason
// `task send` prints the task it is about to hand over: this is the one moment
// the operator authorizes the loss.
func TestTaskDropListConfirmation(t *testing.T) {
	app, st := dropListApp(t)
	old := stdin
	t.Cleanup(func() { stdin = old })

	stdin = strings.NewReader("n\n")
	out, err := runDrop(t, app, dropListAgent, "drop-list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[y/N]") || !strings.Contains(out, "aborted — task list unchanged") {
		t.Errorf("n must abort with the prompt shown, got %q", out)
	}
	if !strings.Contains(out, "2 task(s)") {
		t.Errorf("the prompt must name what is being lost, got %q", out)
	}
	if !strings.Contains(out, "recreates it empty") {
		t.Errorf("the prompt must say a configured source brings it back, got %q", out)
	}
	if listGone(t, st) {
		t.Fatal("an aborted drop deleted the list")
	}

	stdin = strings.NewReader("y\n")
	if out, err = runDrop(t, app, dropListAgent, "drop-list"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "deleted task list") {
		t.Errorf("y must delete and say so, got %q", out)
	}
	if !listGone(t, st) {
		t.Error("a confirmed drop left the list in place")
	}

	// A scripted (non-TTY os.Stdin) run without --yes refuses rather than
	// exiting 0 having done nothing.
	stdin = os.Stdin
	app2, st2 := dropListApp(t)
	if _, err := runDrop(t, app2, dropListAgent, "drop-list"); err == nil ||
		!strings.Contains(err.Error(), "--yes") {
		t.Errorf("non-TTY confirmation must refuse with a --yes hint, got %v", err)
	}
	if listGone(t, st2) {
		t.Error("a refused non-TTY drop deleted the list anyway")
	}
}

// TestTaskDropListYesFlagDeletes: --yes is the scriptable path.
func TestTaskDropListYesFlagDeletes(t *testing.T) {
	app, st := dropListApp(t)
	out, err := runDrop(t, app, dropListAgent, "drop-list", "--yes")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "deleted task list") {
		t.Errorf("missing success message, got %q", out)
	}
	if !listGone(t, st) {
		t.Error("--yes did not delete the list")
	}
}

// TestTaskDropListRefusesAFileBackedList: only a list in the hap database is
// hap's to delete. A file belongs to the operator.
func TestTaskDropListRefusesAFileBackedList(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	app := &frontend.App{Store: st, StateDir: dir,
		ConfigPath: seedLocalFSConfigIn(t, dir), Author: "operator"}
	path := filepath.Join(dir, "tasks.md")
	if err := os.WriteFile(path, []byte("- [ ] alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = runDrop(t, app, "--path", path, "drop-list", "--yes")
	if err == nil {
		t.Fatal("dropping a file-backed list must refuse")
	}
	if !strings.Contains(err.Error(), "not kept in the hap database") {
		t.Errorf("refusal does not name the reason: %v", err)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Errorf("the operator's file was removed: %v", statErr)
	}
}

// TestTaskDropListRefusesAnItemArgument: `remove <n>` is the item-level delete
// on this same verb, so a stray index here must never be read as "drop the list
// and ignore the number". It names the op that WAS meant.
func TestTaskDropListRefusesAnItemArgument(t *testing.T) {
	app, st := dropListApp(t)
	_, err := runDrop(t, app, dropListAgent, "drop-list", "1", "--yes")
	if err == nil {
		t.Fatal("drop-list must refuse an argument")
	}
	if !strings.Contains(err.Error(), "remove <n>") {
		t.Errorf("the refusal must point at the item-level delete: %v", err)
	}
	if listGone(t, st) {
		t.Error("a refused drop deleted the list anyway")
	}
}

// TestTaskDropListMissingListIsNotAnError: the caller asked for it to be gone
// and it is. This is also the shape a cross-node drop takes under the default
// engine, where another machine's row is simply not in this database.
func TestTaskDropListMissingListIsNotAnError(t *testing.T) {
	app, st := dropListApp(t)
	if _, err := runDrop(t, app, dropListAgent, "drop-list", "--yes"); err != nil {
		t.Fatal(err)
	}
	out, err := runDrop(t, app, dropListAgent, "drop-list", "--yes")
	if err != nil {
		t.Fatalf("dropping an already-gone list errored: %v", err)
	}
	if !strings.Contains(out, "nothing to delete") {
		t.Errorf("a second drop should say there was nothing there, got %q", out)
	}
	_ = st
}
