package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

// escapedFixture is a checklist as several markdown editors write one: the dot
// after a leading number is backslash-escaped so the line is not re-rendered as
// an ordered list. The ids deliberately do NOT line up with the positions —
// id "2.1" sits at position 2 and id "3." at position 4 — so any helper that
// quietly falls back to positional addressing hits the wrong item and the
// assertions below catch it.
const escapedFixture = "# Tasks\n\n" +
	"- [ ] 1\\. alpha\n" +
	"- [ ] 2\\.1 beta\n" +
	"- [ ] 2\\.2 gamma\n" +
	"- [x] 3\\. delta\n"

// escapedApp wires a store-backed App to a checklist written with escaped ids,
// plus one idle live agent so `task send` is exercisable.
func escapedApp(t *testing.T) (*frontend.App, *sendRecorderHerdr, string) {
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
	app := &frontend.App{Store: st, Herdr: h,
		ConfigPath: filepath.Join(dir, "config.toml"), Author: "operator"}
	path := filepath.Join(dir, "tasks.md")
	if err := os.WriteFile(path, []byte(escapedFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := app.AddTaskSource(context.Background(), "w1:p1", "", path, ""); err != nil {
		t.Fatal(err)
	}
	return app, h, path
}

// readFixture returns the checklist file's current content.
func readFixture(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// assertNeighborsIntact pins that every item except the one under test still
// carries its ORIGINAL raw markdown, escapes and all. Each op below rewrites
// the whole file, so this is what proves an op touched exactly one line and
// never normalized the operator's escaping away.
func assertNeighborsIntact(t *testing.T, content, step string) {
	t.Helper()
	for _, want := range []string{
		"- [ ] 1\\. alpha",
		"- [ ] 2\\.2 gamma",
		"- [x] 3\\. delta",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("after %s: line %q must be untouched, got:\n%s", step, want, content)
		}
	}
}

// TestEscapedIDLifecycle drives one task through every `hap task` op by its
// escaped id and pins the two properties that make escaped ids safe: the FILE
// keeps the operator's raw markdown (hap ticks boxes, it does not reformat),
// and each op targets exactly the named item — never the item that happens to
// sit at the same position.
func TestEscapedIDLifecycle(t *testing.T) {
	app, h, path := escapedApp(t)
	// Every op below is addressed by the plain id: an operator reads "2.1" in
	// the listing and types "2.1", never the backslash.
	const ref = "2.1"

	// list — the escape is a rendering artifact, so it never reaches the screen.
	out, err := runSend(t, app, "--path", path, "list")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"#1\t[ ]\t1. alpha",
		"#2\t[ ]\t2.1 beta",
		"#3\t[ ]\t2.2 gamma",
		"#4\t[x]\t3. delta",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("list missing %q, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, `\.`) {
		t.Errorf("list must not print the markdown escape, got:\n%s", out)
	}

	// get — resolves the id to its own position (2), not to position 1 (id "1")
	// and not to the "3." item sitting at position 4.
	if out, err = runSend(t, app, "--path", path, "get", ref); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "#2\t[ ]\t2.1 beta") {
		t.Errorf("get %s should print item #2, got:\n%s", ref, out)
	}
	if out, err = runSend(t, app, "--path", path, "get", "3"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "#4\t[x]\t3. delta") {
		t.Errorf("get 3 should reach the item LABELED 3 (position 4), got:\n%s", out)
	}
	// A bare "2" names no id, and position 2 carries one — refusing is the
	// whole point: ticking "2.1" for somebody who asked for task 2 is the
	// mistake the ref rules exist to prevent.
	if _, err = runSend(t, app, "--path", path, "get", "2"); err == nil {
		t.Error("a bare 2 must be refused: no item is labeled 2 and position 2 is labeled")
	}

	// start — marks [-] on the named item only.
	if _, err = runSend(t, app, "--path", path, "start", ref); err != nil {
		t.Fatal(err)
	}
	content := readFixture(t, path)
	if !strings.Contains(content, "- [-] 2\\.1 beta") {
		t.Errorf("start should mark the escaped item [-] and keep its text raw, got:\n%s", content)
	}
	assertNeighborsIntact(t, content, "start")

	// undone — back to pending, so the item is sendable again.
	if _, err = runSend(t, app, "--path", path, "undone", ref); err != nil {
		t.Fatal(err)
	}
	content = readFixture(t, path)
	if !strings.Contains(content, "- [ ] 2\\.1 beta") {
		t.Errorf("undone should return the item to [ ], got:\n%s", content)
	}
	assertNeighborsIntact(t, content, "undone")

	// update — new text, written exactly as typed (the operator may keep the
	// escape, and hap must not "helpfully" strip or add one).
	if _, err = runSend(t, app, "--path", path, "update", ref, `2\.1 beta reworded`); err != nil {
		t.Fatal(err)
	}
	content = readFixture(t, path)
	if !strings.Contains(content, "- [ ] 2\\.1 beta reworded") {
		t.Errorf("update should rewrite only the named item, verbatim, got:\n%s", content)
	}
	assertNeighborsIntact(t, content, "update")

	// send — addressed by AGENT (a --path list has nobody to send to), which
	// also proves the two ways of naming the same source resolve the same item.
	// The agent is the one party that should never see the escape: it reads the
	// id in its prompt and types it back at `hap task done`.
	if _, err = runSend(t, app, "w1:p1", "send", ref, "--yes"); err != nil {
		t.Fatal(err)
	}
	if len(h.sent) != 1 {
		t.Fatalf("expected exactly one delivery, got %v", h.sent)
	}
	if !strings.Contains(h.sent[0], "2.1 beta reworded") || strings.Contains(h.sent[0], `2\.1`) {
		t.Errorf("prompt should carry the unescaped id, got %q", h.sent[0])
	}
	content = readFixture(t, path)
	if !strings.Contains(content, "- [-] 2\\.1 beta reworded") {
		t.Errorf("send should reserve the item as [-] and keep its text raw, got:\n%s", content)
	}
	assertNeighborsIntact(t, content, "send")

	// done — the escaped id still addresses the item now that it is [-].
	if _, err = runSend(t, app, "--path", path, "done", `2\.1`); err != nil {
		t.Fatalf("the escaped spelling must address the same task: %v", err)
	}
	content = readFixture(t, path)
	if !strings.Contains(content, "- [x] 2\\.1 beta reworded") {
		t.Errorf("done should tick the named item, got:\n%s", content)
	}
	assertNeighborsIntact(t, content, "done")

	// remove — deletes that line and nothing else; the remaining items keep
	// their raw text and their own ids.
	if _, err = runSend(t, app, "--path", path, "remove", ref); err != nil {
		t.Fatal(err)
	}
	content = readFixture(t, path)
	if strings.Contains(content, "beta") {
		t.Errorf("remove should delete the named item, got:\n%s", content)
	}
	assertNeighborsIntact(t, content, "remove")
	if want, got := 3, len(domain.ParseChecklist(content)); got != want {
		t.Errorf("checklist should hold %d items after remove, got %d:\n%s", want, got, content)
	}
	// The header and blank line the operator wrote are still there: hap edits
	// checklist lines, never the document around them.
	if !strings.HasPrefix(content, "# Tasks\n\n") {
		t.Errorf("surrounding markdown must survive the lifecycle, got:\n%s", content)
	}
}
