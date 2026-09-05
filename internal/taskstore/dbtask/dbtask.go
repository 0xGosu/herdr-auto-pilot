// Package dbtask implements ports.TaskStore over hap's own database — the
// `sqlite` task-source provider.
//
// A list is a row in the store's task_lists table addressed by
// db://<node>/<name>, so under the default engine it lives in the local
// database beside everything else, and under engine = "turso" it syncs with
// everything else: another machine's TUI reads and edits it through the same
// locator. There is no file, no lock file and no network call here; the store's
// revision compare-and-swap is the whole concurrency story.
package dbtask

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
	"github.com/0xGosu/herdr-auto-pilot/internal/tasklocator"
)

// ErrBlankContent is returned by Ensure for a blank seed. The database could
// store one, but every other backend's create path refuses it and the callers
// rely on the refusal being uniform: a list is created with its header.
var ErrBlankContent = errors.New("refusing to create a blank task list")

// Store reads and writes task lists kept in hap's database.
type Store struct {
	lists ports.TaskListStore
	now   func() time.Time
}

var (
	_ ports.TaskStore     = (*Store)(nil)
	_ ports.EnsureCreator = (*Store)(nil)
)

// New returns the database task store over lists. It deliberately does NOT
// implement ports.RemoteTaskStore: its calls are a local (or in-process) SQL
// round trip, so the daemon reads it inline like a file rather than through
// the remote snapshot machinery.
func New(lists ports.TaskListStore) *Store {
	return &Store{lists: lists, now: time.Now}
}

// Read returns the list's bytes, or an error wrapping fs.ErrNotExist.
func (s *Store) Read(ctx context.Context, locator string) ([]byte, error) {
	ref, err := refOf(locator)
	if err != nil {
		return nil, err
	}
	l, err := s.lists.ReadTaskList(ctx, ref.NodeID, ref.Name)
	if err != nil {
		return nil, err
	}
	return []byte(l.Content), nil
}

// Mutate applies fn as one compare-and-swap read-modify-write. wait is the
// advisory-lock budget of the file backends and has nothing to bound here: a
// lost race re-reads immediately, and the store caps the retries itself.
func (s *Store) Mutate(ctx context.Context, locator string, _ time.Duration,
	fn func(content string) (string, error)) ([]domain.ChecklistItem, error) {

	ref, err := refOf(locator)
	if err != nil {
		return nil, err
	}
	out, err := s.lists.MutateTaskList(ctx, ref.NodeID, ref.Name, s.now(), fn)
	if err != nil {
		return nil, err
	}
	return domain.ParseChecklist(out), nil
}

// Ensure creates the list when it is missing. The agent the list is for is
// recovered from its derived name so the unified Tasks view can label another
// node's list with the agent it feeds.
func (s *Store) Ensure(ctx context.Context, locator, initial string) (bool, error) {
	ref, err := refOf(locator)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(initial) == "" {
		return false, fmt.Errorf("%w: %s", ErrBlankContent, tasklocator.Display(locator))
	}
	return s.lists.EnsureTaskList(ctx, ref.NodeID, ref.Name, agentOf(ref.Name), initial, s.now())
}

func refOf(locator string) (tasklocator.DBRef, error) {
	ref, ok := tasklocator.ParseDB(locator)
	if !ok {
		return tasklocator.DBRef{}, fmt.Errorf("not a database task-list locator: %q", locator)
	}
	return ref, nil
}

// agentOf inverts tasklocator.DerivedFileName for the common case: a name
// ending in ".md" is the agent's. A shared list with an explicit path is
// labelled by that path instead ("" here).
func agentOf(name string) string {
	agent, ok := strings.CutSuffix(name, ".md")
	if !ok {
		return ""
	}
	return agent
}
