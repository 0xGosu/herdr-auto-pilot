// Package gist implements ports.TaskStore over a GitHub gist: each task list
// is one file inside a gist the operator owns.
//
// This is the ONLY package in hap that talks to GitHub about task lists, and
// gist.go is the only file in it that imports anything networked — the egress
// allowlist in internal/privacy names this file by path, so keeping the
// surface to one file is what makes that allowlist meaningful.
//
// Two constraints shape everything here, and both are properties of the API
// rather than choices:
//
//   - PATCH /gists/{id} has NO compare-and-swap. There is no If-Match, no
//     precondition of any kind; ETags on gists are GET-side caching only. So
//     writes are last-write-wins, and hap's advisory file lock — taken on the
//     canonical locator, exactly as for a local file — is what serializes the
//     read-modify-write between processes on this host. A writer that does not
//     take that lock (the gist web UI, `gh gist edit`, hap on another host) can
//     still lose or overwrite an edit. That is documented, not defended
//     against; a check-then-PATCH would narrow the window while reading like a
//     guarantee it cannot make.
//   - GitHub TRUNCATES a large file's content and reports the real size beside
//     it. go-github models no `truncated` flag, so the size mismatch is the
//     only signal, and missing it would mean PATCHing a clipped body back over
//     a complete one. Every read and every mutation refuses on it.
package gist

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/go-github/v90/github"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
	"github.com/0xGosu/herdr-auto-pilot/internal/taskfile"
	"github.com/0xGosu/herdr-auto-pilot/internal/tasklocator"
)

// userAgent identifies hap to GitHub. It carries no operator or machine detail.
const userAgent = "hap-task-store"

// TokenSource supplies the GitHub token for a call. It is a function rather
// than a stored string so the token is read at USE time from the operator's
// env file: a rotated token then applies to the very next call with no restart,
// and no secret is held for the process's lifetime.
type TokenSource func() (string, error)

// Store reads and writes task lists as files inside one gist.
type Store struct {
	gistID  string
	token   TokenSource
	timeout time.Duration

	// http is shared across calls so the connection pool and TLS session
	// survive. The *github.Client is rebuilt per call instead — it is a cheap
	// struct that opens nothing, and rebuilding is what lets the token be read
	// fresh each time.
	http *http.Client

	// baseURL, when set, overrides GitHub's API root. Only tests set it
	// (httptest), because go-github's own baseURL field is unexported and
	// github.WithURLs is the sole seam.
	baseURL string

	// wroteMu/wrote record when this process last WROTE each file, because the
	// gist API is not read-after-write consistent: a GET issued immediately
	// after the PATCH that created a file can still report the file absent
	// (measured live 2026-08-12: 1 miss in 3 create-then-GET rounds). Absent
	// is a load-bearing answer here — it is what Read turns into fs.ErrNotExist
	// and what Ensure turns into a create — so a stale miss makes
	// create-then-use fail: the CLI's create-on-demand add, and the daemon's
	// first hand-out, both write a list and immediately read it back.
	wroteMu sync.Mutex
	wrote   map[string]time.Time

	// settle overrides the re-read schedule; only tests set it, so the unit
	// suite does not spend the production backoff on an unsettled read.
	settle []time.Duration
}

// writeSettleWindow is how long after our own write a "file is absent" answer
// is treated as possibly stale and re-read. Comfortably longer than the lag
// observed in practice, and it costs nothing on the ordinary path: it is
// consulted only when a file we wrote reads back as missing.
const writeSettleWindow = 15 * time.Second

// writeSettleRetries are the re-reads spent inside that window, with a short
// backoff between them.
var writeSettleRetries = []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, 1 * time.Second}

var (
	_ ports.TaskStore       = (*Store)(nil)
	_ ports.EnsureCreator   = (*Store)(nil)
	_ ports.RemoteTaskStore = (*Store)(nil)
)

// Option configures a Store.
type Option func(*Store)

// WithBaseURL points the client at an alternative API root. Tests use it to
// stand up an httptest server; production never sets it.
func WithBaseURL(u string) Option {
	return func(s *Store) { s.baseURL = u }
}

// WithSettleBackoffs replaces the post-write re-read schedule. Tests use it to
// keep an unsettled read fast; production uses the package default.
func WithSettleBackoffs(d []time.Duration) Option {
	return func(s *Store) { s.settle = d }
}

// WithHTTPClient replaces the shared HTTP client (tests supply the httptest
// server's, which trusts its certificate).
func WithHTTPClient(c *http.Client) Option {
	return func(s *Store) { s.http = c }
}

// New returns a store over gistID, reading its token from token at each call.
func New(gistID string, token TokenSource, timeout time.Duration, opts ...Option) *Store {
	s := &Store{gistID: gistID, token: token, timeout: timeout}
	for _, opt := range opts {
		opt(s)
	}
	if s.http == nil {
		// Deliberately a bare client: github.WithTimeout supplies the deadline,
		// so hap never constructs a net.Dialer or a TLS config. That keeps
		// net/url and crypto/tls off the egress allowlist, and keeps the
		// no-remote-dial source scan in internal/privacy satisfied by
		// construction rather than by luck.
		s.http = &http.Client{}
	}
	return s
}

// Remote reports that this store's calls leave the machine, so the daemon reads
// it through a snapshot and mutates it off its main select loop.
func (s *Store) Remote() bool { return true }

// client builds a per-call GitHub client, reading the token now.
func (s *Store) client() (*github.Client, error) {
	tok, err := s.token()
	if err != nil {
		return nil, err
	}
	opts := []github.ClientOptionsFunc{
		github.WithHTTPClient(s.http),
		github.WithAuthToken(tok), // refuses an empty token, which is the fail-closed we want
		github.WithTimeout(s.timeout),
		github.WithUserAgent(userAgent),
	}
	if s.baseURL != "" {
		opts = append(opts, github.WithURLs(&s.baseURL, nil))
	}
	c, err := github.NewClient(opts...)
	if err != nil {
		// Never wrapped with the token in scope.
		return nil, fmt.Errorf("build GitHub client: %w", err)
	}
	return c, nil
}

// fileOf resolves the locator to the file name inside this store's gist.
func (s *Store) fileOf(locator string) (string, error) {
	ref, ok := tasklocator.ParseGist(locator)
	if !ok {
		return "", fmt.Errorf("not a gist task-list locator: %q", locator)
	}
	if ref.GistID != s.gistID {
		return "", fmt.Errorf("locator names gist %s but this store serves %s",
			shortID(ref.GistID), shortID(s.gistID))
	}
	return ref.File, nil
}

// fetch reads the gist and returns one file's content.
//
// found=false means the file is genuinely absent, which callers turn into
// fs.ErrNotExist or into a create. It is returned ONLY when the file list was
// complete — a truncated list cannot distinguish absent from hidden.
func (s *Store) fetch(ctx context.Context, file string) (content string, found bool, err error) {
	content, found, err = s.fetchOnce(ctx, file)
	// Only a file WE wrote, and only while the write is recent, is re-read: a
	// file that never existed still reports absent on the first answer, so a
	// typo'd name fails as fast as it always did.
	if err != nil || found || !s.recentlyWrote(file) {
		return content, found, err
	}
	for _, backoff := range s.settleBackoffs() {
		select {
		case <-ctx.Done():
			return "", false, ctx.Err()
		case <-time.After(backoff):
		}
		// Re-checked each round: once the write is older than the window it
		// is no longer plausibly the API lagging, and the remaining sleeps
		// would only hold Mutate's advisory lock for nothing.
		if !s.recentlyWrote(file) {
			return "", false, nil
		}
		content, found, err = s.fetchOnce(ctx, file)
		if err != nil || found {
			return content, found, err
		}
	}
	return "", false, nil
}

// settleBackoffs is the re-read schedule, overridable per store so a unit test
// can spend microseconds instead of seconds.
func (s *Store) settleBackoffs() []time.Duration {
	if len(s.settle) > 0 {
		return s.settle
	}
	return writeSettleRetries
}

// recentlyWrote reports whether this process wrote the file inside the settle
// window, i.e. whether "absent" could be the API lagging our own write.
func (s *Store) recentlyWrote(file string) bool {
	s.wroteMu.Lock()
	defer s.wroteMu.Unlock()
	at, ok := s.wrote[file]
	return ok && time.Since(at) < writeSettleWindow
}

func (s *Store) markWrote(file string) {
	s.wroteMu.Lock()
	defer s.wroteMu.Unlock()
	if s.wrote == nil {
		s.wrote = map[string]time.Time{}
	}
	now := time.Now()
	// Every entry is dead once its window passes, so the map stays the size of
	// the lists written in the last few seconds rather than growing for the
	// life of a long-running daemon.
	for f, at := range s.wrote {
		if now.Sub(at) >= writeSettleWindow {
			delete(s.wrote, f)
		}
	}
	s.wrote[file] = now
}

func (s *Store) fetchOnce(ctx context.Context, file string) (content string, found bool, err error) {
	c, err := s.client()
	if err != nil {
		return "", false, err
	}
	g, _, err := c.Gists.Get(ctx, s.gistID)
	if err != nil {
		return "", false, wrapf(s.gistID, err, "read gist %s", shortID(s.gistID))
	}
	f, ok := g.Files[github.GistFilename(file)]
	if !ok {
		if len(g.Files) >= gistFileCeiling {
			return "", false, ErrFileListTruncated
		}
		return "", false, nil
	}
	body := f.GetContent()
	if truncated(f.GetSize(), body) {
		return "", false, fmt.Errorf("%q in gist %s: %w", file, shortID(s.gistID), ErrTruncated)
	}
	return body, true, nil
}

// put writes one file's content, leaving every other file in the gist alone.
//
// The Files map carries ONLY the target: go-github deletes a file when its
// entry maps to nil, so a request rebuilt from a Get — with entries for files
// whose content was truncated or omitted — could clobber or delete another
// agent's list.
func (s *Store) put(ctx context.Context, file, content string) error {
	c, err := s.client()
	if err != nil {
		return err
	}
	_, _, err = c.Gists.Update(ctx, s.gistID, github.UpdateGistRequest{
		Files: map[github.GistFilename]*github.UpdateGistFile{
			github.GistFilename(file): {Content: github.Ptr(content)},
		},
	})
	if err != nil {
		return wrapf(s.gistID, err, "write %q to gist %s", file, shortID(s.gistID))
	}
	s.markWrote(file)
	return nil
}

// Read returns the task list's bytes, or an error wrapping fs.ErrNotExist when
// the gist holds no such file.
func (s *Store) Read(ctx context.Context, locator string) ([]byte, error) {
	file, err := s.fileOf(locator)
	if err != nil {
		return nil, err
	}
	content, found, err := s.fetch(ctx, file)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("task list %q in gist %s: %w", file, shortID(s.gistID), fs.ErrNotExist)
	}
	return []byte(content), nil
}

// Mutate applies fn as one read-modify-write, serialized by hap's advisory lock.
//
// The lock is the whole concurrency story: gists have no compare-and-swap, so
// this is what stops two hap processes on this host from overwriting each
// other. The GET happens INSIDE the lock, so hap never writes based on a
// snapshot older than that GET.
func (s *Store) Mutate(ctx context.Context, locator string, wait time.Duration,
	fn func(content string) (string, error)) ([]domain.ChecklistItem, error) {

	file, err := s.fileOf(locator)
	if err != nil {
		return nil, err
	}

	unlock, err := s.lock(locator, wait)
	if err != nil {
		return nil, err
	}
	defer unlock()

	content, found, err := s.fetch(ctx, file)
	if err != nil {
		return nil, err
	}
	if !found {
		// Same contract as the local store: Mutate never creates. A caller that
		// should create one calls Ensure first, so a typo'd file name fails
		// loudly instead of minting an empty checklist beside the real one.
		return nil, fmt.Errorf("task list %q in gist %s: %w", file, shortID(s.gistID), fs.ErrNotExist)
	}

	out, err := fn(content)
	if err != nil {
		// Nothing is written. The mutator's error is the caller's answer.
		return nil, err
	}
	if out == content {
		// Nothing changed, so spend no request. This also keeps a no-op edit
		// from touching the gist's updated_at, which an operator reads as
		// "something happened".
		return domain.ParseChecklist(out), nil
	}
	if err := s.put(ctx, file, out); err != nil {
		return nil, err
	}
	return domain.ParseChecklist(out), nil
}

// Ensure creates the task list inside the gist when it is missing.
func (s *Store) Ensure(ctx context.Context, locator, initial string) (bool, error) {
	file, err := s.fileOf(locator)
	if err != nil {
		return false, err
	}

	unlock, err := s.lock(locator, 0)
	if err != nil {
		return false, err
	}
	defer unlock()

	_, found, err := s.fetch(ctx, file)
	if err != nil {
		return false, err
	}
	if found {
		return false, nil
	}
	// "Absent" after the settle re-reads, for a file THIS process wrote, means
	// the API has still not caught up — not that the list is gone. Creating
	// here would PATCH the initial content (often empty) over a populated
	// list, which is exactly the overwrite EnsureCreator promises never to do.
	// Fail instead: the caller retries or reports, and no content is lost.
	if s.recentlyWrote(file) {
		return false, fmt.Errorf("task list %q in gist %s was written by this process but still reads as absent — "+
			"the gist API has not caught up; retry shortly", file, shortID(s.gistID))
	}
	if err := s.put(ctx, file, initial); err != nil {
		return false, err
	}
	return true, nil
}

// lock takes hap's advisory lock for this list. Keyed on the canonical locator,
// so every hap process on this host — daemon, CLI, TUI — serializes on the same
// file regardless of which one is talking to GitHub.
func (s *Store) lock(locator string, wait time.Duration) (func(), error) {
	lockPath := taskfile.LockPath(locator)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return nil, err
	}
	return taskfile.LockWithin(lockPath, wait)
}

// EnvFileTokenSource reads the token from a KEY=VALUE env file at each call.
//
// It reuses the loader the LLM adapter uses for its own credentials, so the
// secrecy guarantees are the same ones already reviewed: values are never
// logged, never echoed, and never included in an error message — a failure
// names the file and a line number only.
func EnvFileTokenSource(path string, load func(string) ([]string, error), keys ...string) TokenSource {
	return func() (string, error) {
		if path == "" {
			return "", errors.New("[task_source_provider] env_file is not set — it must name a " +
				"file holding a GitHub token with the `gist` scope")
		}
		entries, err := load(path)
		if err != nil {
			// load already names the file and line only, never content.
			return "", err
		}
		for _, key := range keys {
			prefix := key + "="
			for _, e := range entries {
				if len(e) > len(prefix) && e[:len(prefix)] == prefix {
					if v := e[len(prefix):]; v != "" {
						return v, nil
					}
				}
			}
		}
		return "", fmt.Errorf("no GitHub token in %s — it must set one of %v to a token with "+
			"the `gist` scope", path, keys)
	}
}
