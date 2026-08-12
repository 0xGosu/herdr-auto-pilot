package frontend_test

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

// fakeRemoteStore is an in-process REMOTE task store, keyed by locator.
//
// It exists so this package can test the remote provider at all. Every other
// test here runs on a local file, where a locator happens to be a path — and a
// locator handled as a path is the one failure mode that cannot show up under
// that condition. It reports Remote() = true and refuses anything that is not a
// `gist://` locator, so code that hands it a filesystem path fails the way the
// real backend does rather than quietly working.
type fakeRemoteStore struct {
	mu    sync.Mutex
	files map[string]string
	// mutations records each locator a write landed on, in order.
	mutations []string
}

var (
	_ ports.TaskStore       = (*fakeRemoteStore)(nil)
	_ ports.EnsureCreator   = (*fakeRemoteStore)(nil)
	_ ports.RemoteTaskStore = (*fakeRemoteStore)(nil)
)

func newFakeRemoteStore() *fakeRemoteStore {
	return &fakeRemoteStore{files: map[string]string{}}
}

func (s *fakeRemoteStore) Remote() bool { return true }

// check refuses a locator this backend could never serve. The real gist store
// does the same (fileOf), and it is what turns "handed a local path" into a
// visible failure instead of an accidental success.
func (s *fakeRemoteStore) check(locator string) error {
	if !strings.HasPrefix(locator, "gist://") {
		return fmt.Errorf("not a gist task-list locator: %q", locator)
	}
	return nil
}

func (s *fakeRemoteStore) Read(_ context.Context, locator string) ([]byte, error) {
	if err := s.check(locator); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	content, ok := s.files[locator]
	if !ok {
		return nil, fmt.Errorf("task list %q: %w", locator, fs.ErrNotExist)
	}
	return []byte(content), nil
}

func (s *fakeRemoteStore) Mutate(ctx context.Context, locator string, _ time.Duration,
	fn func(string) (string, error)) ([]domain.ChecklistItem, error) {

	// A remote store uses the caller's ctx for its calls, so a cancelled one
	// means the write does not happen. Honored here deliberately: the local
	// backend ignores ctx, which is why a rollback inheriting a dead ctx was
	// invisible to every other test in this package.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.check(locator); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	content, ok := s.files[locator]
	if !ok {
		return nil, fmt.Errorf("task list %q: %w", locator, fs.ErrNotExist)
	}
	out, err := fn(content)
	if err != nil {
		return nil, err
	}
	if out != content {
		s.files[locator] = out
		s.mutations = append(s.mutations, locator)
	}
	return domain.ParseChecklist(out), nil
}

func (s *fakeRemoteStore) Ensure(_ context.Context, locator, initial string) (bool, error) {
	if err := s.check(locator); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.files[locator]; ok {
		return false, nil
	}
	s.files[locator] = initial
	return true, nil
}

func (s *fakeRemoteStore) content(locator string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.files[locator]
}

func (s *fakeRemoteStore) put(locator, content string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[locator] = content
}

func (s *fakeRemoteStore) writes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.mutations...)
}

// cancellingHerdr fails a send AND cancels the confirm's context, standing in
// for the operator quitting the TUI mid-delivery.
type cancellingHerdr struct {
	fakeHerdr
	cancel context.CancelFunc
	err    error
}

func (h *cancellingHerdr) Send(context.Context, string, string) error {
	h.cancel()
	return h.err
}

// remoteApp returns an App configured for a github_gist provider with the fake
// store behind it. The LOCATOR is still minted by the real tasklocator, so the
// `gist://<id>/<file>` shape under test is the production one.
func remoteApp(t *testing.T) (*frontend.App, *store.Store, *fakeRemoteStore) {
	t.Helper()
	app, st := testApp(t)
	app.Herdr = &fakeHerdr{}
	app.StateDir = t.TempDir()
	remote := newFakeRemoteStore()
	app.TaskStoreFor = func(config.Config, string) (ports.TaskStore, error) { return remote, nil }
	// env_file is set to a real file so the fixture is a config the REAL
	// registry would accept: the seam bypasses ValidateResolvedProvider, and a
	// fixture that only works because of the bypass would keep passing if the
	// store lookup itself regressed.
	envFile := filepath.Join(t.TempDir(), "gist.env")
	if err := os.WriteFile(envFile, []byte("GITHUB_TOKEN=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, kv := range [][2]string{
		{"task_source_provider.provider", "github_gist"},
		{"task_source_provider.github_gist.gist_id", "3f2a1b9c"},
		{"task_source_provider.env_file", envFile},
	} {
		if _, err := app.SetField(context.Background(), kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	return app, st, remote
}

// TestRemoteConfirmReservesThroughTheStore is the regression guard for the
// send-time reservation.
//
// Confirming a generated task WITH a send reserves the item ("[-]") and only
// then delivers. That reservation used taskfile.Mutate — a local os.Stat, read
// and atomic write — while the surrounding read and write had already moved to
// the store. Under a gist provider `path` is a `gist://…` URI, so the stat could
// only fail, and it failed AFTER the list was written, the source registered and
// the escalation CLAIMED: the escalation was consumed, the tasks sat in the
// list, and nothing was ever delivered.
//
// The fake store refuses a non-gist locator, so a reservation that goes back to
// treating the locator as a path fails here the way it failed live.
func TestRemoteConfirmReservesThroughTheStore(t *testing.T) {
	app, st, remote := remoteApp(t)
	ctx := context.Background()

	name, _ := st.EnsureAgentName(ctx, "w1:p1")
	id, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w1:p1", SituationType: domain.SituationIdle, Trigger: "t",
		Action: "escalated", Status: "escalated",
		Suggestion: domain.SuggestTaskPrefix + "Do the thing", CreatedAt: time.Now(),
	})
	if err := app.Confirm(ctx, id, true); err != nil {
		t.Fatalf("confirm with a send must succeed against a remote store: %v", err)
	}

	locator := "gist://3f2a1b9c/" + name + ".md"
	got := remote.content(locator)
	if got == "" {
		t.Fatalf("nothing was written to %s; store holds %v", locator, remote.writes())
	}
	// Reserved in the STORE, which is the whole point: the send happened, so
	// item 1 must be "[-]" where the agent's list actually lives.
	if !strings.Contains(got, "- [-] 1. Do the thing") {
		t.Errorf("task 1 was not reserved in the remote list:\n%s", got)
	}
	if len(app.Herdr.(*fakeHerdr).inputs) == 0 {
		t.Error("the task was never delivered to the agent")
	}
}

// TestRemoteConfirmRollsBackThroughTheStore: when the send fails, the item
// returns to "[ ]" in the store rather than being stranded at "[-]".
//
// The rollback is a compensating write, and it must not inherit the cancellation
// that most often caused the failure it compensates for — a remote store uses
// the caller's ctx for its calls, so a cancelled confirm would fail the send AND
// the release, leaving the operator's list stuck.
func TestRemoteConfirmRollsBackThroughTheStore(t *testing.T) {
	app, st, remote := remoteApp(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The send fails BY being cancelled, which is the realistic pairing: the
	// operator quits the TUI or hits Ctrl-C, so the delivery dies and the ctx
	// the rollback would inherit is already dead. A rollback on the live ctx
	// therefore cannot run at all, precisely when the item most needs releasing.
	app.Herdr = &cancellingHerdr{cancel: cancel, err: fmt.Errorf("pane is gone")}

	name, _ := st.EnsureAgentName(ctx, "w2:p2")
	id, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w2:p2", SituationType: domain.SituationIdle, Trigger: "t",
		Action: "escalated", Status: "escalated",
		Suggestion: domain.SuggestTaskPrefix + "Do the thing", CreatedAt: time.Now(),
	})
	err := app.Confirm(ctx, id, true)
	if err == nil {
		t.Fatal("a failed send must be reported")
	}
	if strings.Contains(err.Error(), "could not be returned to [ ]") {
		t.Errorf("the rollback itself failed, stranding the item: %v", err)
	}

	locator := "gist://3f2a1b9c/" + name + ".md"
	got := remote.content(locator)
	if strings.Contains(got, "[-]") {
		t.Errorf("the item is stranded in progress after a failed send:\n%s", got)
	}
	if !strings.Contains(got, "- [ ] 1. Do the thing") {
		t.Errorf("the item did not return to pending:\n%s", got)
	}
}

// TestRemoteAppendTargetReadsCandidatesThroughTheStore is the regression guard
// for pickAppendTarget.
//
// It chooses among an agent's matched sources by CONTENT — first with a pending
// item, else first with any items, else first in config order — and read each
// candidate with os.ReadFile on `src.Path`. A gist source's path is a bare file
// name inside the gist, so every read failed; a failed read reads as "no
// content", so the precedence silently collapsed to "always the first source".
// Wrong list, no error, nothing logged.
func TestRemoteAppendTargetReadsCandidatesThroughTheStore(t *testing.T) {
	app, st, remote := remoteApp(t)
	ctx := context.Background()

	name, _ := st.EnsureAgentName(ctx, "w3:p3")
	// Two declared gist sources for this agent, in config order. Only the
	// SECOND holds pending work, so it must win.
	for _, file := range []string{"empty.md", "has-pending.md"} {
		if err := app.AddTaskSource(ctx, name, "", file, ""); err != nil {
			t.Fatal(err)
		}
	}
	remote.put("gist://3f2a1b9c/empty.md", "# Tasks\n")
	remote.put("gist://3f2a1b9c/has-pending.md", "# Tasks\n- [ ] 1. existing pending\n")

	id, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w3:p3", SituationType: domain.SituationIdle, Trigger: "t",
		Action: "escalated", Status: "escalated",
		Suggestion: domain.SuggestTaskPrefix + "Generated task", CreatedAt: time.Now(),
	})
	if err := app.Confirm(ctx, id, false); err != nil {
		t.Fatal(err)
	}

	inEmpty := strings.Contains(remote.content("gist://3f2a1b9c/empty.md"), "Generated task")
	inPending := strings.Contains(remote.content("gist://3f2a1b9c/has-pending.md"), "Generated task")
	if inEmpty && inPending {
		t.Fatal("the task was appended to BOTH lists; exactly one is the target")
	}
	if !inPending {
		t.Errorf("the task must go to the source holding pending work, not the first in config "+
			"order — a candidate read that cannot resolve a gist source degrades to "+
			"'always the first' with nothing reported.\nempty.md:\n%s\nhas-pending.md:\n%s",
			remote.content("gist://3f2a1b9c/empty.md"), remote.content("gist://3f2a1b9c/has-pending.md"))
	}
}

// TestRemoteDerivedSourceStillTakesTheAppendPath: a DERIVED remote source
// (`path` unset — the documented one-list-per-agent form) is somebody's
// declared source and must receive the tasks by APPEND, even though it resolves
// to the very same file name the bootstrap would have registered.
//
// This is the trap in identifying the bootstrap list by locator alone:
// `DerivedFileName(agent)` is byte-identical to what the bootstrap writes, so
// equality excludes the derived source, `pickAppendTarget` sees nothing, and the
// bootstrap flow runs — at which point addTaskSourceIfAbsent refuses to register
// a second source for the agent and the confirm fails permanently, AFTER
// bootstrap-numbered items have been written into the operator's real list.
func TestRemoteDerivedSourceStillTakesTheAppendPath(t *testing.T) {
	app, st, remote := remoteApp(t)
	ctx := context.Background()

	name, _ := st.EnsureAgentName(ctx, "w5:p5")
	// Path deliberately empty: the provider derives "<agent>.md".
	if err := app.AddTaskSource(ctx, name, "", "", ""); err != nil {
		t.Fatal(err)
	}
	locator := "gist://3f2a1b9c/" + name + ".md"
	remote.put(locator, "# Tasks\n- [ ] 1. existing pending\n")

	id, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w5:p5", SituationType: domain.SituationIdle, Trigger: "t",
		Action: "escalated", Status: "escalated",
		Suggestion: domain.SuggestTaskPrefix + "Generated task", CreatedAt: time.Now(),
	})
	if err := app.Confirm(ctx, id, false); err != nil {
		t.Fatalf("a derived remote source must be appended to, not mistaken for the "+
			"bootstrap list: %v", err)
	}

	got := remote.content(locator)
	if !strings.Contains(got, "existing pending") {
		t.Errorf("the existing task was lost:\n%s", got)
	}
	if !strings.Contains(got, "Generated task") {
		t.Errorf("the generated task was not appended:\n%s", got)
	}
	// Still exactly one source: a second would break `hap task <agent>`.
	cfg, err := config.Load(app.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.TaskSources) != 1 {
		t.Errorf("got %d task sources, want 1: %+v", len(cfg.TaskSources), cfg.TaskSources)
	}
}

// TestRemoteConfirmAppendsDespiteABrokenRemoteDefault: a misconfigured remote
// DEFAULT must not fail a confirm that a healthy declared source can serve.
//
// Resolving the bootstrap list moved ABOVE the exclusion loop (the comparison
// needs it), and returning its error there would break the append path, which
// never needed that resolution at all. Here the default provider has no
// gist_id, while the agent's own source overrides the provider to local_fs.
func TestRemoteConfirmAppendsDespiteABrokenRemoteDefault(t *testing.T) {
	app, st := testApp(t)
	app.Herdr = &fakeHerdr{}
	app.StateDir = t.TempDir()
	ctx := context.Background()

	// A remote default with NO gist_id: the bootstrap list cannot resolve.
	if _, err := app.SetField(ctx, "task_source_provider.provider", "github_gist"); err != nil {
		t.Fatal(err)
	}

	name, _ := st.EnsureAgentName(ctx, "w6:p6")
	listPath := filepath.Join(t.TempDir(), "declared.md")
	if err := os.WriteFile(listPath, []byte("# Tasks\n- [ ] 1. existing pending\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	localOverride := frontend.TaskSourceOption(func(src *config.TaskSource) { src.Provider = "local_fs" })
	if err := app.AddTaskSource(ctx, name, "", listPath, "", localOverride); err != nil {
		t.Fatal(err)
	}

	id, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w6:p6", SituationType: domain.SituationIdle, Trigger: "t",
		Action: "escalated", Status: "escalated",
		Suggestion: domain.SuggestTaskPrefix + "Generated task", CreatedAt: time.Now(),
	})
	if err := app.Confirm(ctx, id, false); err != nil {
		t.Fatalf("a healthy declared source must still be appended to when the remote "+
			"DEFAULT is misconfigured: %v", err)
	}
	got, _ := os.ReadFile(listPath)
	if !strings.Contains(string(got), "Generated task") {
		t.Errorf("the task was not appended to the declared source:\n%s", got)
	}
}

// TestRemoteSecondConfirmDoesNotDuplicateItsOwnList: the agent's OWN bootstrap
// list must never be treated as an external append target.
//
// The exclusion compared a local path against each candidate's `src.Path`.
// Under a gist provider the registered source carries a bare file name that
// absolutizes to <cwd>/<name>.md and never matched, so on a SECOND confirm the
// agent's own list was classified as external and took appendGeneratedTasks —
// whose dedupe keys on raw task text while the bootstrap flow stores NUMBERED
// items ("1. Do X"). Every task already listed was therefore appended again,
// un-numbered.
func TestRemoteSecondConfirmDoesNotDuplicateItsOwnList(t *testing.T) {
	app, st, remote := remoteApp(t)
	ctx := context.Background()

	name, _ := st.EnsureAgentName(ctx, "w4:p4")
	for _, n := range []int{1, 2} {
		id, _ := st.AppendAudit(ctx, domain.AuditRecord{
			AgentID: "w4:p4", SituationType: domain.SituationIdle, Trigger: "t",
			Action: "escalated", Status: "escalated",
			Suggestion: domain.SuggestTaskPrefix + "Alpha task", CreatedAt: time.Now(),
		})
		if err := app.Confirm(ctx, id, false); err != nil {
			t.Fatalf("confirm %d: %v", n, err)
		}
	}

	got := remote.content("gist://3f2a1b9c/" + name + ".md")
	if n := strings.Count(got, "Alpha task"); n != 1 {
		t.Errorf("the task appears %d times after re-confirming the same suggestion, want 1 — "+
			"the agent's own list was treated as an external append target:\n%s", n, got)
	}
	// And exactly one source is registered: a second one would break
	// `hap task <agent>` with a "matches 2 task sources" ambiguity.
	cfg, err := config.Load(app.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.TaskSources) != 1 {
		t.Errorf("got %d task sources, want 1: %+v", len(cfg.TaskSources), cfg.TaskSources)
	}
}
