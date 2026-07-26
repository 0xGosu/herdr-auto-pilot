package updatecheck

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fetchFrom points Latest at a test server. The production URL is a const, so
// the test drives the same request-building and parsing through a client whose
// transport rewrites the destination.
func fetchFrom(t *testing.T, handler http.HandlerFunc) (string, error) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	client := srv.Client()
	base := srv.URL
	client.Transport = rewriteTransport{base: base, next: srv.Client().Transport}
	return Latest(context.Background(), client)
}

// rewriteTransport sends every request to the test server instead of GitHub.
type rewriteTransport struct {
	base string
	next http.RoundTripper
}

func (r rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u := *req.URL
	target, err := http.NewRequest(req.Method, r.base+u.Path, req.Body)
	if err != nil {
		return nil, err
	}
	target.Header = req.Header
	return r.next.RoundTrip(target.WithContext(req.Context()))
}

func TestLatestParsesTag(t *testing.T) {
	var gotAccept string
	tag, err := fetchFrom(t, func(w http.ResponseWriter, req *http.Request) {
		gotAccept = req.Header.Get("Accept")
		w.Write([]byte(`{"tag_name":"v0.5.2","name":"0.5.2"}`))
	})
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if tag != "v0.5.2" {
		t.Errorf("tag = %q, want %q", tag, "v0.5.2")
	}
	if gotAccept != "application/vnd.github+json" {
		t.Errorf("Accept header = %q", gotAccept)
	}
}

// TestLatestFailures pins that every remote misbehavior is a plain error for
// the caller to record — never a panic, never a bogus version.
func TestLatestFailures(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"rate limited", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}},
		{"server error", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}},
		{"malformed body", func(w http.ResponseWriter, _ *http.Request) {
			w.Write([]byte("<html>not json</html>"))
		}},
		{"missing tag", func(w http.ResponseWriter, _ *http.Request) {
			w.Write([]byte(`{"name":"0.5.2"}`))
		}},
		{"unusable tag", func(w http.ResponseWriter, _ *http.Request) {
			w.Write([]byte(`{"tag_name":"nightly"}`))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tag, err := fetchFrom(t, tc.handler)
			if err == nil {
				t.Fatalf("expected an error, got tag %q", tag)
			}
			if tag != "" {
				t.Errorf("failed fetch returned a tag: %q", tag)
			}
		})
	}
}

// blockedTransport fails every request, so this test cannot reach the network
// even if the cancellation it is checking for stops working.
type blockedTransport struct{}

func (blockedTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("network blocked in tests")
}

// TestLatestCancelledContext keeps the check from outliving its caller.
func TestLatestCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Latest(ctx, &http.Client{Transport: blockedTransport{}}); err == nil {
		t.Error("expected an error for a cancelled context")
	}
}

// TestRepoMatchesInstallTarget keeps the checked repo and the documented
// upgrade command in sync — they must name the same project.
func TestRepoMatchesInstallTarget(t *testing.T) {
	if !strings.Contains(latestReleaseURL, Repo) {
		t.Errorf("release URL %q does not target %q", latestReleaseURL, Repo)
	}
}
