package tui

import (
	"context"
	"strings"
	"testing"
)

// TestTasksRemoveSourceOffersDeletingADatabaseList: x on the header of a
// source whose list lives in the hap database offers a SECOND answer, D, that
// also deletes the list — while y keeps meaning "keep it", so the heavier
// action is never what a reflexive enter does.
func TestTasksRemoveSourceOffersDeletingADatabaseList(t *testing.T) {
	m, app, st := dropListModel(t)
	m = press(t, m, "x") // the cursor starts on the header row
	if m.confirm == nil {
		t.Fatalf("x on a retirable header should ask, got message %q", m.message)
	}
	if m.confirm.alt == nil || m.confirm.alt.key != "D" {
		t.Fatalf("a database list should offer D, got %+v", m.confirm.alt)
	}
	if !strings.Contains(m.confirm.label, "D also deletes the list and its 2 task(s)") {
		t.Errorf("the label must name both answers and what D loses, got %q", m.confirm.label)
	}
	if help := m.helpLine(); !strings.Contains(help, "D: also delete the list") {
		t.Errorf("the help line must name the D answer, got %q", help)
	}

	upd, cmd := m.Update(pressKeyMsg("D"))
	m, res := runAction(t, upd.(Model), cmd)
	if res.err != nil {
		t.Fatalf("remove + delete failed: %v", res.err)
	}
	if m.status == nil || !strings.Contains(m.status.text, "task source #0 removed and task list") {
		t.Errorf("success should name both halves, got %+v", m.status)
	}
	cfg, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.TaskSources) != 0 {
		t.Errorf("the source should be gone, got %+v", cfg.TaskSources)
	}
	if !dropListGone(t, st) {
		t.Error("D left the list in place")
	}
}

// TestTasksRemoveSourceYKeepsTheDatabaseList is the control for the case
// above: the same prompt answered with y removes only the source.
func TestTasksRemoveSourceYKeepsTheDatabaseList(t *testing.T) {
	m, app, st := dropListModel(t)
	m = press(t, m, "x")
	if m.confirm == nil {
		t.Fatal("expected a confirmation")
	}
	upd, cmd := m.Update(pressKeyMsg("y"))
	if _, res := runAction(t, upd.(Model), cmd); res.err != nil {
		t.Fatalf("removal failed: %v", res.err)
	}
	if cfg, _ := app.Config(); len(cfg.TaskSources) != 0 {
		t.Errorf("the source should be gone, got %+v", cfg.TaskSources)
	}
	if dropListGone(t, st) {
		t.Error("y must keep the list")
	}
}

// TestTasksRemoveSourceWithholdsDForASharedList: another source hands out the
// same list, so deleting it would empty a live queue. D is not offered, the
// label says why, and pressing it anyway does nothing.
func TestTasksRemoveSourceWithholdsDForASharedList(t *testing.T) {
	m, app, st := dropListModel(t)
	ctx := context.Background()
	if err := app.AddTaskSource(ctx, "calm-heron", "", "shared.md", ""); err != nil {
		t.Fatal(err)
	}
	upd, _ := m.Update(refreshData(ctx, app))
	m = upd.(Model)
	m = press(t, m, "x")
	if m.confirm == nil {
		t.Fatalf("expected a confirmation, got message %q", m.message)
	}
	if m.confirm.alt != nil {
		t.Fatalf("a shared list must not offer D, got %+v", m.confirm.alt)
	}
	if !strings.Contains(m.confirm.label, "task source #1 uses it too") {
		t.Errorf("the label must say why D is missing, got %q", m.confirm.label)
	}
	upd, cmd := m.Update(pressKeyMsg("D"))
	m = upd.(Model)
	if cmd != nil || m.confirm == nil {
		t.Error("D must do nothing when it was not offered")
	}
	if dropListGone(t, st) {
		t.Error("a shared list was deleted")
	}
}

// TestTasksRemoveSourceNeverOffersDForAFile: a file on disk is the operator's.
func TestTasksRemoveSourceNeverOffersDForAFile(t *testing.T) {
	m, _, _ := taskAppModel(t)
	m = press(t, m, "x")
	if m.confirm == nil {
		t.Fatalf("expected a confirmation, got message %q", m.message)
	}
	if m.confirm.alt != nil {
		t.Errorf("a file-backed source must not offer D, got %+v", m.confirm.alt)
	}
	if !strings.Contains(m.confirm.label, "checklist file is kept") {
		t.Errorf("label = %q", m.confirm.label)
	}
}
