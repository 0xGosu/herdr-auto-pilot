package dbtask_test

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
	"github.com/0xGosu/herdr-auto-pilot/internal/taskstore/dbtask"
)

// fakeLists is an in-memory ports.TaskListStore with the store's CAS contract.
type fakeLists struct {
	mu    sync.Mutex
	node  string
	lists map[string]*domain.StoredTaskList // "node/name"
}

func newFakeLists(node string) *fakeLists {
	return &fakeLists{node: node, lists: map[string]*domain.StoredTaskList{}}
}

func (f *fakeLists) NodeID() string { return f.node }

func (f *fakeLists) ReadTaskList(_ context.Context, node, name string) (domain.StoredTaskList, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.lists[node+"/"+name]
	if !ok {
		return domain.StoredTaskList{}, fs.ErrNotExist
	}
	return *l, nil
}

func (f *fakeLists) MutateTaskList(_ context.Context, node, name string, now time.Time,
	fn func(string) (string, error)) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.lists[node+"/"+name]
	if !ok {
		return "", fs.ErrNotExist
	}
	out, err := fn(l.Content)
	if err != nil {
		return "", err
	}
	if out != l.Content {
		l.Content, l.Revision, l.UpdatedAt = out, l.Revision+1, now
	}
	return out, nil
}

func (f *fakeLists) EnsureTaskList(_ context.Context, node, name, agent, initial string, now time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.lists[node+"/"+name]; ok {
		return false, nil
	}
	f.lists[node+"/"+name] = &domain.StoredTaskList{NodeID: node, Name: name, AgentName: agent, Content: initial, Revision: 1, UpdatedAt: now}
	return true, nil
}

func (f *fakeLists) ListTaskLists(context.Context) ([]domain.StoredTaskList, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.StoredTaskList
	for _, l := range f.lists {
		out = append(out, *l)
	}
	return out, nil
}

const node = "a1a1a1a1a1a1a1a1"

func TestDBTaskReadMutateEnsure(t *testing.T) {
	ctx := context.Background()
	lists := newFakeLists(node)
	s := dbtask.New(lists)
	loc := "db://" + node + "/brave-otter.md"

	if _, err := s.Read(ctx, loc); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Read of a missing list must wrap fs.ErrNotExist, got %v", err)
	}
	if _, err := s.Mutate(ctx, loc, 0, func(c string) (string, error) { return c, nil }); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Mutate never creates: got %v", err)
	}

	created, err := s.Ensure(ctx, loc, "# Tasks for brave-otter\n\n- [ ] alpha\n")
	if err != nil || !created {
		t.Fatalf("Ensure = (%v, %v)", created, err)
	}
	l, _ := lists.ReadTaskList(ctx, node, "brave-otter.md")
	if l.AgentName != "brave-otter" {
		t.Errorf("the derived name must label the list with its agent, got %q", l.AgentName)
	}

	got, err := s.Read(ctx, loc)
	if err != nil || !strings.Contains(string(got), "- [ ] alpha") {
		t.Fatalf("Read = %q, %v", got, err)
	}
	items, err := s.Mutate(ctx, loc, 0, func(c string) (string, error) {
		return domain.MarkChecklistItemInProgress(c, 1)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Mark != domain.MarkInProgress {
		t.Errorf("Mutate returned %+v, want the parsed post-mutation list", items)
	}
}

func TestDBTaskRefusesABlankSeedAndForeignLocators(t *testing.T) {
	ctx := context.Background()
	s := dbtask.New(newFakeLists(node))
	loc := "db://" + node + "/x.md"
	for _, blank := range []string{"", "\n", "   "} {
		if _, err := s.Ensure(ctx, loc, blank); !errors.Is(err, dbtask.ErrBlankContent) {
			t.Errorf("Ensure(%q) = %v, want ErrBlankContent", blank, err)
		}
	}
	for _, bad := range []string{"/tmp/tasks.md", "gist://abc/x.md", "db://" + node} {
		if _, err := s.Read(ctx, bad); err == nil || errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Read(%q) = %v, want a refusal that is NOT a missing-list error", bad, err)
		}
	}
}

// TestDBTaskIsNotARemoteStore pins that the daemon reads it inline like a file:
// a database round trip is local, so the remote snapshot machinery (and its
// staleness) must not apply.
func TestDBTaskIsNotARemoteStore(t *testing.T) {
	var s ports.TaskStore = dbtask.New(newFakeLists(node))
	if ports.TaskStoreRemote(s) {
		t.Error("dbtask must not declare RemoteTaskStore")
	}
	if _, ok := s.(ports.EnsureCreator); !ok {
		t.Error("dbtask must create on demand")
	}
}
