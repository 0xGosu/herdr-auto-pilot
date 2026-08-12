package gist_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
	"github.com/0xGosu/herdr-auto-pilot/internal/taskstore/gist"
)

const (
	testGistID = "3f2a1b9c4d5e6f708192a3b4c5d6e7f8"
	testFile   = "brave-otter.md"
)

func locator(file string) string { return "gist://" + testGistID + "/" + file }

// fakeGist is a minimal stand-in for the two endpoints this backend uses. It
// records every request so tests can assert on request COUNT and PATCH BODY,
// which is where the safety properties live.
type fakeGist struct {
	mu sync.Mutex
	// files is the gist's current content, keyed by file name.
	files map[string]string
	// sizes overrides the reported size of a file, to simulate truncation.
	sizes map[string]int
	// extraFiles pads the file count, to simulate the listing ceiling.
	extraFiles int

	gets    int
	patches int
	// patchBodies records each PATCH's decoded files map.
	patchBodies []map[string]any

	// hideAfterPatch counts GETs that still omit a just-PATCHed file, which is
	// how the real API behaves: gist reads are not read-after-write
	// consistent, so a GET right after a create can miss it.
	hideAfterPatch int
	hidden         map[string]bool

	// getStatus / patchStatus, when non-zero, make that verb fail.
	getStatus, patchStatus int
	getBody, patchBody     string

	// authHeaders records the Authorization header of every request.
	authHeaders []string
}

func newFakeGist(files map[string]string) *fakeGist {
	f := &fakeGist{files: map[string]string{}, sizes: map[string]int{}}
	for k, v := range files {
		f.files[k] = v
	}
	return f
}

func (f *fakeGist) counts() (get, patch int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gets, f.patches
}

func (f *fakeGist) content(name string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.files[name]
	return v, ok
}

func (f *fakeGist) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/gists/"+testGistID, func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.authHeaders = append(f.authHeaders, r.Header.Get("Authorization"))
		f.mu.Unlock()

		switch r.Method {
		case http.MethodGet:
			f.mu.Lock()
			f.gets++
			status, body := f.getStatus, f.getBody
			f.mu.Unlock()
			if status != 0 {
				http.Error(w, body, status)
				return
			}
			f.writeGist(w)
		case http.MethodPatch:
			raw, _ := io.ReadAll(r.Body)
			var req struct {
				Files map[string]any `json:"files"`
			}
			_ = json.Unmarshal(raw, &req)

			f.mu.Lock()
			f.patches++
			f.patchBodies = append(f.patchBodies, req.Files)
			status, body := f.patchStatus, f.patchBody
			if status == 0 {
				for name, v := range req.Files {
					m, ok := v.(map[string]any)
					if !ok || m == nil {
						delete(f.files, name)
						continue
					}
					if c, ok := m["content"].(string); ok {
						f.files[name] = c
						if f.hideAfterPatch > 0 {
							if f.hidden == nil {
								f.hidden = map[string]bool{}
							}
							f.hidden[name] = true
						}
						delete(f.sizes, name)
					}
				}
			}
			f.mu.Unlock()
			if status != 0 {
				http.Error(w, body, status)
				return
			}
			f.writeGist(w)
		default:
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	})
	return mux
}

func (f *fakeGist) writeGist(w http.ResponseWriter) {
	f.mu.Lock()
	defer f.mu.Unlock()
	files := map[string]any{}
	for name, content := range f.files {
		// Simulate the API's read-after-write lag for a file we just wrote.
		if f.hideAfterPatch > 0 && f.hidden[name] {
			f.hideAfterPatch--
			if f.hideAfterPatch == 0 {
				f.hidden = nil
			}
			continue
		}
		size := len(content)
		if s, ok := f.sizes[name]; ok {
			size = s
		}
		files[name] = map[string]any{
			"filename": name,
			"size":     size,
			"content":  content,
		}
	}
	for i := range f.extraFiles {
		name := fmt.Sprintf("pad-%03d.md", i)
		files[name] = map[string]any{"filename": name, "size": 1, "content": "x"}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"id": testGistID, "files": files})
}

// newStore stands up the fake and a store pointed at it.
func newStore(t *testing.T, f *fakeGist) *gist.Store {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	// The trailing slash is REQUIRED: go-github's NewRequest hard-errors
	// without it ("baseURL must have a trailing slash").
	base := srv.URL + "/"
	return gist.New(testGistID, func() (string, error) { return "test-token", nil },
		5*time.Second, gist.WithBaseURL(base), gist.WithHTTPClient(srv.Client()))
}

func TestGistReadReturnsTheFilesContent(t *testing.T) {
	f := newFakeGist(map[string]string{testFile: "- [ ] alpha\n- [ ] beta\n"})
	got, err := newStore(t, f).Read(context.Background(), locator(testFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "- [ ] alpha\n- [ ] beta\n" {
		t.Errorf("content = %q", got)
	}
}

func TestGistMissingFileReadsAsErrNotExist(t *testing.T) {
	f := newFakeGist(map[string]string{"other.md": "- [ ] x\n"})
	_, err := newStore(t, f).Read(context.Background(), locator(testFile))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a missing file must wrap fs.ErrNotExist so callers can decide to create it; got %v", err)
	}
}

// TestGistReadRefusesTruncatedContent and its Mutate sibling guard the only
// failure here that DESTROYS data.
func TestGistReadRefusesTruncatedContent(t *testing.T) {
	f := newFakeGist(map[string]string{testFile: "- [ ] alpha\n"})
	f.sizes[testFile] = 999999 // GitHub reports the real size beside a clipped body

	_, err := newStore(t, f).Read(context.Background(), locator(testFile))
	if !errors.Is(err, gist.ErrTruncated) {
		t.Fatalf("want ErrTruncated, got %v", err)
	}
	if _, patches := f.counts(); patches != 0 {
		t.Errorf("a truncated READ issued %d PATCH requests", patches)
	}
}

func TestGistMutateRefusesTruncatedContentAndWritesNothing(t *testing.T) {
	f := newFakeGist(map[string]string{testFile: "- [ ] alpha\n"})
	f.sizes[testFile] = 999999

	_, err := newStore(t, f).Mutate(context.Background(), locator(testFile), 0,
		func(c string) (string, error) { return c + "- [ ] appended\n", nil })
	if !errors.Is(err, gist.ErrTruncated) {
		t.Fatalf("want ErrTruncated, got %v", err)
	}
	if _, patches := f.counts(); patches != 0 {
		t.Errorf("writing back a truncated body would DELETE the rest of the list; "+
			"got %d PATCH requests", patches)
	}
	if got, _ := f.content(testFile); got != "- [ ] alpha\n" {
		t.Errorf("the stored list changed: %q", got)
	}
}

// TestGistRefusesAFileListAtTheTruncationCeiling: a partial file list makes
// "absent" and "hidden" indistinguishable, and they have opposite responses.
func TestGistRefusesAFileListAtTheTruncationCeiling(t *testing.T) {
	f := newFakeGist(map[string]string{"other.md": "x"})
	f.extraFiles = 400

	s := newStore(t, f)
	if _, err := s.Read(context.Background(), locator(testFile)); !errors.Is(err, gist.ErrFileListTruncated) {
		t.Errorf("Read: want ErrFileListTruncated, got %v", err)
	}
	if _, err := s.Ensure(context.Background(), locator(testFile), "# Tasks\n"); !errors.Is(err, gist.ErrFileListTruncated) {
		t.Errorf("Ensure must not create a file it cannot prove is absent; got %v", err)
	}
	if _, patches := f.counts(); patches != 0 {
		t.Errorf("nothing may be written in this state; got %d PATCH requests", patches)
	}
}

// TestGistMutateIsOneGetThenOnePatchOfOnlyTheTargetFile is a safety test, not a
// performance one: go-github DELETES a gist file when its entry maps to nil, so
// a PATCH rebuilt from a Get could clobber or delete another agent's list.
func TestGistMutateIsOneGetThenOnePatchOfOnlyTheTargetFile(t *testing.T) {
	f := newFakeGist(map[string]string{
		testFile:            "- [ ] alpha\n",
		"calm-badger.md":    "- [ ] someone else's work\n",
		"shared-backlog.md": "- [ ] shared\n",
	})
	s := newStore(t, f)

	if _, err := s.Mutate(context.Background(), locator(testFile), 0,
		func(c string) (string, error) { return c + "- [ ] beta\n", nil }); err != nil {
		t.Fatal(err)
	}

	gets, patches := f.counts()
	if gets != 1 || patches != 1 {
		t.Errorf("got %d GET and %d PATCH, want exactly 1 each", gets, patches)
	}
	f.mu.Lock()
	body := f.patchBodies[0]
	f.mu.Unlock()
	if len(body) != 1 {
		t.Fatalf("the PATCH names %d files, want only the target: %v", len(body), body)
	}
	if _, ok := body[testFile]; !ok {
		t.Errorf("the PATCH does not name %q: %v", testFile, body)
	}
	for _, other := range []string{"calm-badger.md", "shared-backlog.md"} {
		got, ok := f.content(other)
		if !ok {
			t.Errorf("%s was DELETED by a mutation of another file", other)
			continue
		}
		if strings.Contains(got, "beta") {
			t.Errorf("%s was overwritten: %q", other, got)
		}
	}
}

func TestGistMutateReturnsTheMutatorsErrorAndWritesNothing(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(string) (string, error)
		want   string
	}{
		{"plain refusal", func(string) (string, error) { return "REPLACED", errors.New("refused") }, "refused"},
		{
			"a claim guard refusing",
			func(string) (string, error) { return "", errors.New("the checklist changed; refresh and retry") },
			"checklist changed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGist(map[string]string{testFile: "- [ ] alpha\n"})
			_, err := newStore(t, f).Mutate(context.Background(), locator(testFile), 0, tc.mutate)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error mentioning %q, got %v", tc.want, err)
			}
			if _, patches := f.counts(); patches != 0 {
				t.Errorf("a mutator error must write ZERO bytes; got %d PATCH requests", patches)
			}
			if got, _ := f.content(testFile); got != "- [ ] alpha\n" {
				t.Errorf("content changed: %q", got)
			}
		})
	}
}

func TestGistMutateSkipsThePatchWhenNothingChanged(t *testing.T) {
	f := newFakeGist(map[string]string{testFile: "- [ ] alpha\n"})
	items, err := newStore(t, f).Mutate(context.Background(), locator(testFile), 0,
		func(c string) (string, error) { return c, nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Errorf("got %d items, want the parsed list back regardless", len(items))
	}
	if _, patches := f.counts(); patches != 0 {
		t.Errorf("a no-op edit must not touch the gist (it would move updated_at); "+
			"got %d PATCH requests", patches)
	}
}

func TestGistMutateDoesNotRetryAFailedPatch(t *testing.T) {
	f := newFakeGist(map[string]string{testFile: "- [ ] alpha\n"})
	f.patchStatus, f.patchBody = http.StatusInternalServerError, `{"message":"boom"}`

	_, err := newStore(t, f).Mutate(context.Background(), locator(testFile), 0,
		func(c string) (string, error) { return c + "- [ ] beta\n", nil })
	if err == nil {
		t.Fatal("a failed PATCH must be reported")
	}
	if _, patches := f.counts(); patches != 1 {
		t.Errorf("a failed write must NOT be retried — it may have landed; got %d PATCH requests", patches)
	}
}

// TestGistFailuresAreErrorsNotEmptyLists: a 401 reading as "no pending tasks"
// would silently stop every hand-out with nothing in the audit trail.
func TestGistFailuresAreErrorsNotEmptyLists(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"unauthorized", http.StatusUnauthorized, `{"message":"Bad credentials"}`},
		{"forbidden", http.StatusForbidden, `{"message":"rate limit exceeded"}`},
		{"not found", http.StatusNotFound, `{"message":"Not Found"}`},
		{"rate limited", http.StatusTooManyRequests, `{"message":"slow down"}`},
		{"server error", http.StatusInternalServerError, `{"message":"boom"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGist(map[string]string{testFile: "- [ ] alpha\n"})
			f.getStatus, f.getBody = tc.status, tc.body
			s := newStore(t, f)

			got, err := s.Read(context.Background(), locator(testFile))
			if err == nil {
				t.Fatalf("want an error, got content %q", got)
			}
			if errors.Is(err, fs.ErrNotExist) {
				t.Error("a transport/auth failure must NOT read as an absent list — a caller " +
					"would create one beside the real one")
			}
			if _, mErr := s.Mutate(context.Background(), locator(testFile), 0,
				func(c string) (string, error) { return c, nil }); mErr == nil {
				t.Error("Mutate must fail too")
			}
		})
	}
}

func TestGistEnsureCreatesTheFileOnDemandAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	f := newFakeGist(map[string]string{})
	s := newStore(t, f)

	created, err := s.Ensure(ctx, locator(testFile), "# Tasks for brave-otter\n\n")
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Error("the first Ensure must report that it created the list")
	}
	if got, ok := f.content(testFile); !ok || !strings.Contains(got, "# Tasks for brave-otter") {
		t.Errorf("the file was not created: %q", got)
	}

	// Content arrives, then a second Ensure must leave it alone.
	if _, err := s.Mutate(ctx, locator(testFile), 0, func(c string) (string, error) {
		out, _, aerr := domain.AppendChecklistItem(c, "real work")
		return out, aerr
	}); err != nil {
		t.Fatal(err)
	}
	created, err = s.Ensure(ctx, locator(testFile), "# Tasks for brave-otter\n\n")
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("the second Ensure must report that it created nothing")
	}
	if got, _ := f.content(testFile); !strings.Contains(got, "real work") {
		t.Errorf("Ensure overwrote an existing list: %q", got)
	}
}

func TestGistClientSendsTheBearerTokenFromTheTokenSource(t *testing.T) {
	f := newFakeGist(map[string]string{testFile: "- [ ] alpha\n"})
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	base := srv.URL + "/"

	calls := 0
	s := gist.New(testGistID, func() (string, error) {
		calls++
		return fmt.Sprintf("tok-%d", calls), nil
	}, 5*time.Second, gist.WithBaseURL(base), gist.WithHTTPClient(srv.Client()))

	for range 2 {
		if _, err := s.Read(context.Background(), locator(testFile)); err != nil {
			t.Fatal(err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.authHeaders) != 2 {
		t.Fatalf("got %d requests, want 2", len(f.authHeaders))
	}
	// Read at USE time, per call: a rotated token applies to the next call with
	// no restart, and no secret is held for the process's lifetime.
	if f.authHeaders[0] != "Bearer tok-1" || f.authHeaders[1] != "Bearer tok-2" {
		t.Errorf("auth headers = %v, want the token re-read per call", f.authHeaders)
	}
}

func TestGistRefusesAnEmptyToken(t *testing.T) {
	f := newFakeGist(map[string]string{testFile: "- [ ] alpha\n"})
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	base := srv.URL + "/"

	s := gist.New(testGistID, func() (string, error) { return "", nil },
		5*time.Second, gist.WithBaseURL(base), gist.WithHTTPClient(srv.Client()))
	if _, err := s.Read(context.Background(), locator(testFile)); err == nil {
		t.Fatal("an empty token must fail closed rather than making an unauthenticated call")
	}
	if gets, _ := f.counts(); gets != 0 {
		t.Errorf("no request may be made without a token; got %d", gets)
	}
}

// TestGistErrorsNeverContainTheTokenOrTheFullGistID: a secret gist's id is
// effectively a capability URL, and go-github embeds the request URL in its
// error text.
func TestGistErrorsNeverContainTheTokenOrTheFullGistID(t *testing.T) {
	const token = "ghp_supersecrettokenvalue"
	cases := []struct {
		name   string
		status int
	}{
		{"unauthorized", http.StatusUnauthorized},
		{"not found", http.StatusNotFound},
		{"server error", http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGist(map[string]string{testFile: "- [ ] alpha\n"})
			f.getStatus, f.getBody = tc.status, `{"message":"nope"}`
			srv := httptest.NewServer(f.handler())
			t.Cleanup(srv.Close)
			base := srv.URL + "/"

			s := gist.New(testGistID, func() (string, error) { return token, nil },
				5*time.Second, gist.WithBaseURL(base), gist.WithHTTPClient(srv.Client()))

			_, err := s.Read(context.Background(), locator(testFile))
			if err == nil {
				t.Fatal("want an error")
			}
			if strings.Contains(err.Error(), token) {
				t.Errorf("the error leaks the token: %v", err)
			}
			if strings.Contains(err.Error(), testGistID) {
				t.Errorf("the error leaks the full gist id (a secret gist's URL is a "+
					"capability): %v", err)
			}
			if !strings.Contains(err.Error(), testGistID[:8]) {
				t.Errorf("the error should still name a recognizable prefix: %v", err)
			}
		})
	}
}

// TestGistStoreNeverTouchesTheFilesystemBeyondItsLock: a remote store must not
// quietly fall back to a local file, which would fork the list in two.
func TestGistStoreNeverTouchesTheFilesystemBeyondItsLock(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) })
	if err := os.Chdir(dir); err != nil {
		t.Skipf("cannot chdir: %v", err)
	}

	f := newFakeGist(map[string]string{testFile: "- [ ] alpha\n"})
	s := newStore(t, f)
	ctx := context.Background()
	if _, err := s.Read(ctx, locator(testFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Mutate(ctx, locator(testFile), 0,
		func(c string) (string, error) { return c + "- [ ] beta\n", nil }); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("the gist store wrote %q into the working directory", filepath.Join(dir, e.Name()))
	}
}

// TestGistStoreNeverFallsBackToLocal: with the API failing, a list that exists
// on disk under the same name must NOT be read.
func TestGistStoreNeverFallsBackToLocal(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) })
	if err := os.Chdir(dir); err != nil {
		t.Skipf("cannot chdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, testFile), []byte("- [ ] LOCAL DECOY\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	f := newFakeGist(map[string]string{})
	f.getStatus, f.getBody = http.StatusInternalServerError, `{"message":"boom"}`
	got, err := newStore(t, f).Read(context.Background(), locator(testFile))
	if err == nil {
		t.Fatalf("the store returned %q instead of failing — a silent local fallback would "+
			"fork the list into two divergent copies", got)
	}
	if strings.Contains(string(got), "LOCAL DECOY") {
		t.Error("the store read a local file")
	}
}

func TestGistStoreDeclaresItselfRemote(t *testing.T) {
	s := gist.New(testGistID, func() (string, error) { return "t", nil }, time.Second)
	if !ports.TaskStoreRemote(s) {
		t.Error("the gist store must declare itself remote, or the daemon would run its " +
			"network round trips on the main select loop")
	}
}

func TestGistRefusesALocatorForAnotherGist(t *testing.T) {
	f := newFakeGist(map[string]string{testFile: "- [ ] alpha\n"})
	s := newStore(t, f)
	for _, bad := range []string{
		"gist://someothergist/brave-otter.md",
		"/home/me/tasks.md",
		"",
	} {
		if _, err := s.Read(context.Background(), bad); err == nil {
			t.Errorf("Read(%q) must be refused", bad)
		}
	}
	if gets, _ := f.counts(); gets != 0 {
		t.Errorf("a locator this store does not serve must be refused before any request; got %d", gets)
	}
}

func TestEnvFileTokenSource(t *testing.T) {
	load := func(entries []string, err error) func(string) ([]string, error) {
		return func(string) ([]string, error) { return entries, err }
	}
	cases := []struct {
		name    string
		path    string
		entries []string
		loadErr error
		want    string
		wantErr string
	}{
		{
			name: "reads the primary key", path: "/etc/hap/task.env",
			entries: []string{"GITHUB_TOKEN=ghp_abc"}, want: "ghp_abc",
		},
		{
			name: "falls back to the secondary key", path: "/etc/hap/task.env",
			entries: []string{"GH_TOKEN=ghp_def"}, want: "ghp_def",
		},
		{
			name: "prefers the primary key", path: "/etc/hap/task.env",
			entries: []string{"GH_TOKEN=second", "GITHUB_TOKEN=first"}, want: "first",
		},
		{
			name: "an unset path is refused", path: "",
			wantErr: "env_file is not set",
		},
		{
			name: "an empty value is not a token", path: "/etc/hap/task.env",
			entries: []string{"GITHUB_TOKEN="}, wantErr: "no GitHub token",
		},
		{
			name: "no matching key", path: "/etc/hap/task.env",
			entries: []string{"UNRELATED=x"}, wantErr: "no GitHub token",
		},
		{
			name: "a load failure propagates", path: "/etc/hap/task.env",
			loadErr: errors.New("task.env line 3: malformed"), wantErr: "line 3",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := gist.EnvFileTokenSource(tc.path, load(tc.entries, tc.loadErr), "GITHUB_TOKEN", "GH_TOKEN")
			got, err := src()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want an error mentioning %q, got %v", tc.wantErr, err)
				}
				// A failure names the file, never a value.
				for _, entry := range tc.entries {
					if _, v, ok := strings.Cut(entry, "="); ok && v != "" && strings.Contains(err.Error(), v) {
						t.Errorf("the error leaks a value from the env file: %v", err)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("token = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestGistReadSettlesAfterOurOwnWrite: gist reads are NOT read-after-write
// consistent — a GET issued right after the PATCH that created a file can
// still report it absent (measured live against api.github.com: one miss in
// three create-then-GET rounds). "Absent" is load-bearing here: Read turns it
// into fs.ErrNotExist and Ensure turns it into a create, so a stale miss broke
// create-then-use — the CLI's create-on-demand add and the daemon's first
// hand-out both write a list and immediately read it back.
func TestGistReadSettlesAfterOurOwnWrite(t *testing.T) {
	f := newFakeGist(nil)
	f.hideAfterPatch = 2 // the next two GETs pretend the new file is not there
	s := newStore(t, f)

	created, err := s.Ensure(context.Background(), locator(testFile), "# Tasks\n")
	if err != nil || !created {
		t.Fatalf("Ensure: created=%v err=%v", created, err)
	}
	// Immediately after our own write: the first reads miss, and the store
	// must re-read rather than report the list absent.
	got, err := s.Read(context.Background(), locator(testFile))
	if err != nil {
		t.Fatalf("a read right after our own create must settle, got %v", err)
	}
	if string(got) != "# Tasks\n" {
		t.Errorf("content = %q, want the created list", got)
	}
}

// TestGistReadDoesNotWaitForAFileWeNeverWrote: the settle re-read is scoped to
// files THIS process wrote. A name that never existed — a typo — must fail on
// the first answer, as it always did, rather than costing a backoff.
func TestGistReadDoesNotWaitForAFileWeNeverWrote(t *testing.T) {
	f := newFakeGist(map[string]string{testFile: "- [ ] a\n"})
	s := newStore(t, f)
	if _, err := s.Read(context.Background(), locator("typo.md")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want fs.ErrNotExist", err)
	}
	if f.gets != 1 {
		t.Errorf("GETs = %d, want exactly 1 — a never-written file must not be re-read", f.gets)
	}
}
