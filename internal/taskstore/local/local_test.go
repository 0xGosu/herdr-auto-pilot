package local_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
	"github.com/0xGosu/herdr-auto-pilot/internal/taskfile"
	"github.com/0xGosu/herdr-auto-pilot/internal/taskstore/local"
)

// appendItem adapts domain.AppendChecklistItem (which also returns the new
// index) to the mutator shape the store takes.
func appendItem(text string) func(string) (string, error) {
	return func(c string) (string, error) {
		out, _, err := domain.AppendChecklistItem(c, text)
		return out, err
	}
}

func writeList(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tasks.md")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestLocalStoreMatchesTaskfileMutateExactly is the no-regression guard for the
// default provider: every call must land on internal/taskfile with the same
// arguments and the same results, so wiring the store in changes nothing.
func TestLocalStoreMatchesTaskfileMutateExactly(t *testing.T) {
	ctx := context.Background()
	s := local.New()

	cases := []struct {
		name    string
		body    string
		mutate  func(string) (string, error)
		want    string
		wantErr string
	}{
		{
			name: "reserve marks the item in progress",
			body: "- [ ] alpha\n- [ ] beta\n",
			mutate: func(c string) (string, error) {
				return domain.MarkChecklistItemInProgress(c, 1)
			},
			want: "- [-] alpha\n- [ ] beta\n",
		},
		{
			name:   "append adds an item",
			body:   "- [ ] alpha\n",
			mutate: appendItem("beta"),
			want:   "- [ ] alpha\n- [ ] beta\n",
		},
		{
			name:    "a mutator error writes nothing",
			body:    "- [ ] alpha\n",
			mutate:  func(string) (string, error) { return "REPLACED", errors.New("refused") },
			wantErr: "refused",
			want:    "- [ ] alpha\n",
		},
		{
			name:    "ExpectText refuses a stale index",
			body:    "- [ ] alpha\n",
			mutate:  taskfile.Reserve(1, "not what is there"),
			wantErr: "checklist changed",
			want:    "- [ ] alpha\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := writeList(t, tc.body)
			_, err := s.Mutate(ctx, p, 0, tc.mutate)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("want an error mentioning %q", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q must mention %q", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			got, rerr := s.Read(ctx, p)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if string(got) != tc.want {
				t.Errorf("content = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReadReportsAMissingListAsErrNotExist(t *testing.T) {
	s := local.New()
	_, err := s.Read(context.Background(), filepath.Join(t.TempDir(), "absent.md"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a missing list must wrap fs.ErrNotExist — callers branch on it to decide "+
			"whether to create one; got %v", err)
	}
}

func TestMutateRefusesAMissingListRatherThanCreatingIt(t *testing.T) {
	s := local.New()
	p := filepath.Join(t.TempDir(), "absent.md")
	if _, err := s.Mutate(context.Background(), p, 0, func(c string) (string, error) {
		return c + "- [ ] injected\n", nil
	}); err == nil {
		t.Fatal("Mutate must refuse a list that does not exist, so a typo'd path fails loudly " +
			"instead of silently minting an empty checklist")
	}
	if _, err := os.Stat(p); err == nil {
		t.Error("Mutate created the file")
	}
}

func TestMutatePreservesExistingPermissions(t *testing.T) {
	p := writeList(t, "- [ ] alpha\n")
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := local.New().Mutate(context.Background(), p, 0, appendItem("beta")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Errorf("mode = %o, want 0644 — an operator's --path checklist must not be narrowed "+
			"on every edit", got)
	}
}

func TestEnsureIsIdempotentAndNeverClobbers(t *testing.T) {
	ctx := context.Background()
	s := local.New()
	p := filepath.Join(t.TempDir(), "sub", "tasks.md")

	created, err := s.Ensure(ctx, p, "# Tasks for brave-otter\n\n")
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Error("the first Ensure must report that it created the list")
	}

	// Content arrives after creation; a second Ensure must not touch it.
	if _, err := s.Mutate(ctx, p, 0, appendItem("real work")); err != nil {
		t.Fatal(err)
	}
	created, err = s.Ensure(ctx, p, "# Tasks for brave-otter\n\n")
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("the second Ensure must report that it created nothing")
	}
	body, err := s.Read(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "real work") {
		t.Errorf("Ensure overwrote an existing list: %q", body)
	}
}

// TestEnsureIsAtomicUnderConcurrentCreates is the fix for a live TOCTOU: the
// caller this replaces did a bare os.Stat then an os.WriteFile, so two
// concurrent generated-task confirms could both miss the stat and both write,
// with the second discarding the first's content.
func TestEnsureIsAtomicUnderConcurrentCreates(t *testing.T) {
	ctx := context.Background()
	s := local.New()
	p := filepath.Join(t.TempDir(), "tasks.md")

	const n = 8
	var wg sync.WaitGroup
	results := make([]bool, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i], errs[i] = s.Ensure(ctx, p, "# Tasks\n\n")
		}()
	}
	close(start)
	wg.Wait()

	creators := 0
	for i := range n {
		if errs[i] != nil {
			t.Errorf("goroutine %d: %v", i, errs[i])
		}
		if results[i] {
			creators++
		}
	}
	if creators != 1 {
		t.Errorf("exactly one Ensure must report created=true, got %d", creators)
	}
}

func TestLocalStoreIsNotRemote(t *testing.T) {
	if ports.TaskStoreRemote(local.New()) {
		t.Error("the local store must not declare itself remote, or the daemon would move " +
			"every local task-list read off its main loop for no reason")
	}
}
