package herdr

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/fakeherdr"
	"github.com/0xGosu/herdr-auto-pilot/internal/testutil"
)

func newTestNotifier(t *testing.T) (*SocketNotifier, *fakeherdr.Server) {
	t.Helper()
	srv, err := fakeherdr.NewServer(testutil.SocketDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return NewSocketNotifier(srv.SocketPath), srv
}

func TestSocketNotifierShowsNotification(t *testing.T) {
	n, srv := newTestNotifier(t)

	res, err := n.ShowNotification(context.Background(), "build failed", "api workspace")
	if err != nil {
		t.Fatalf("ShowNotification: %v", err)
	}
	if !res.Shown || res.Reason != "shown" {
		t.Fatalf("want a displayed toast, got %+v", res)
	}

	got := srv.SocketNotifications()
	if len(got) != 1 {
		t.Fatalf("want 1 notification recorded, got %d", len(got))
	}
	if got[0].Title != "build failed" || got[0].Body != "api workspace" {
		t.Errorf("title/body not delivered: %+v", got[0])
	}
	// The operator-attention sound is what distinguishes an escalation toast
	// from an informational one.
	if got[0].Sound != SoundRequest {
		t.Errorf("sound = %q, want %q", got[0].Sound, SoundRequest)
	}
}

// TestSocketNotifierNotShownIsNotAnError pins the contract the TUI's bell
// fallback depends on: herdr answering "I dropped it" is a successful call
// reporting Shown=false, never an error. Collapsing the two would make a
// rate-limited toast indistinguishable from a broken socket.
func TestSocketNotifierNotShownIsNotAnError(t *testing.T) {
	n, srv := newTestNotifier(t)
	srv.SetNotificationResult(false, "rate_limited")

	res, err := n.ShowNotification(context.Background(), "escalation", "agent blocked")
	if err != nil {
		t.Fatalf("a declined toast must not be an error, got %v", err)
	}
	if res.Shown {
		t.Error("Shown should be false")
	}
	if res.Reason != "rate_limited" {
		t.Errorf("Reason = %q, want %q", res.Reason, "rate_limited")
	}
}

func TestSocketNotifierProtocolError(t *testing.T) {
	n, srv := newTestNotifier(t)
	srv.SetNotificationError("invalid_params", "title must contain visible text")

	res, err := n.ShowNotification(context.Background(), "x", "y")
	if err == nil {
		t.Fatal("want an error for a protocol-level failure")
	}
	if res.Shown {
		t.Error("a failed call must not report Shown")
	}
	var se *SocketError
	if !errors.As(err, &se) {
		t.Fatalf("want a *SocketError, got %T: %v", err, err)
	}
	if se.Code != "invalid_params" || se.Method != "notification.show" {
		t.Errorf("unexpected SocketError: %+v", se)
	}
	if !strings.Contains(err.Error(), "title must contain visible text") {
		t.Errorf("error should name herdr's message, got %q", err.Error())
	}
}

func TestSocketNotifierUnreachableSocket(t *testing.T) {
	n := NewSocketNotifier(filepath.Join(testutil.SocketDir(t), "no-such.sock"))
	n.Timeout = 200 * time.Millisecond

	start := time.Now()
	res, err := n.ShowNotification(context.Background(), "title", "body")
	if err == nil {
		t.Fatal("want an error when the socket does not exist")
	}
	if res.Shown {
		t.Error("a failed call must not report Shown")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("dial should fail fast, took %v", elapsed)
	}
}

// TestSocketNotifierEmptyTitleRejectedLocally: herdr rejects an empty
// normalized title with invalid_params, so we catch it before opening a
// connection — and a whitespace-only title normalizes to empty.
func TestSocketNotifierEmptyTitleRejectedLocally(t *testing.T) {
	n, srv := newTestNotifier(t)

	for _, title := range []string{"", "   ", "\n\t "} {
		if _, err := n.ShowNotification(context.Background(), title, "body"); err == nil {
			t.Errorf("title %q should be rejected", title)
		}
	}
	if got := srv.SocketNotifications(); len(got) != 0 {
		t.Errorf("no request should have been sent, got %d", len(got))
	}
}

func TestSocketNotifierClampsText(t *testing.T) {
	n, srv := newTestNotifier(t)

	longTitle := strings.Repeat("a", 200)
	longBody := strings.Repeat("b", 500)
	if _, err := n.ShowNotification(context.Background(), longTitle, longBody); err != nil {
		t.Fatal(err)
	}
	got := srv.SocketNotifications()
	if len(got) != 1 {
		t.Fatalf("want 1 notification, got %d", len(got))
	}
	if n := len([]rune(got[0].Title)); n != maxNotifyTitle {
		t.Errorf("title length = %d, want %d", n, maxNotifyTitle)
	}
	if n := len([]rune(got[0].Body)); n != maxNotifyBody {
		t.Errorf("body length = %d, want %d", n, maxNotifyBody)
	}
}

// TestSocketNotifierCollapsesWhitespace: a pane excerpt or a multi-line
// suggestion must reach herdr as the single line it will paint, so what we
// clamp is what it shows.
func TestSocketNotifierCollapsesWhitespace(t *testing.T) {
	n, srv := newTestNotifier(t)

	if _, err := n.ShowNotification(context.Background(),
		" agent  blocked \n", "line one\nline two\t\ttabbed  "); err != nil {
		t.Fatal(err)
	}
	got := srv.SocketNotifications()[0]
	if got.Title != "agent blocked" {
		t.Errorf("title = %q, want %q", got.Title, "agent blocked")
	}
	if got.Body != "line one line two tabbed" {
		t.Errorf("body = %q, want %q", got.Body, "line one line two tabbed")
	}
}

// TestSocketNotifierOmitsEmptyOptionalParams: herdr validates `sound` and
// `position` against a fixed set, so an empty string must be left out of the
// request rather than sent as "".
func TestSocketNotifierOmitsEmptyOptionalParams(t *testing.T) {
	n, srv := newTestNotifier(t)

	if _, err := n.Show(context.Background(), Notification{Title: "bare"}); err != nil {
		t.Fatal(err)
	}
	got := srv.SocketNotifications()[0]
	if got.Body != "" || got.Sound != "" || got.Position != "" {
		t.Errorf("optional params should be omitted when empty, got %+v", got)
	}
}

// TestSocketNotifierRequestIDsAreUnique: herdr echoes the request id back,
// so reusing one across calls would make two responses indistinguishable.
func TestSocketNotifierRequestIDsAreUnique(t *testing.T) {
	n, srv := newTestNotifier(t)

	for i := 0; i < 3; i++ {
		if _, err := n.ShowNotification(context.Background(), "t", "b"); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	for _, got := range srv.SocketNotifications() {
		if got.ID == "" {
			t.Fatal("request carried no id")
		}
		if seen[got.ID] {
			t.Fatalf("request id %q was reused", got.ID)
		}
		seen[got.ID] = true
	}
	if len(seen) != 3 {
		t.Errorf("want 3 distinct ids, got %d", len(seen))
	}
}

// TestSocketNotifierConcurrentCalls: the TUI can dispatch an escalation toast
// and a pause toast from the same refresh, on separate goroutines.
func TestSocketNotifierConcurrentCalls(t *testing.T) {
	n, srv := newTestNotifier(t)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := n.ShowNotification(context.Background(), "t", "b"); err != nil {
				t.Errorf("ShowNotification: %v", err)
			}
		}()
	}
	wg.Wait()

	seen := map[string]bool{}
	for _, got := range srv.SocketNotifications() {
		if seen[got.ID] {
			t.Fatalf("concurrent calls reused request id %q", got.ID)
		}
		seen[got.ID] = true
	}
	if len(seen) != 8 {
		t.Errorf("want 8 distinct ids, got %d", len(seen))
	}
}

func TestSocketNotifierNotifyIgnoresNotShown(t *testing.T) {
	n, srv := newTestNotifier(t)
	srv.SetNotificationResult(false, "disabled")

	// The plain NotifyPort form has no channel for "shown"; a dropped toast
	// must not surface as a transport error there either.
	if err := n.Notify(context.Background(), "title", "body"); err != nil {
		t.Fatalf("Notify: %v", err)
	}
}

func TestSocketPath(t *testing.T) {
	t.Setenv("HERDR_SOCKET_PATH", "/run/herdr/custom.sock")
	if got := SocketPath(); got != "/run/herdr/custom.sock" {
		t.Errorf("SocketPath() = %q, want the injected value", got)
	}

	t.Setenv("HERDR_SOCKET_PATH", "")
	got := SocketPath()
	if got == "" || !strings.HasSuffix(got, filepath.Join(".config", "herdr", "herdr.sock")) {
		t.Errorf("SocketPath() = %q, want the default session socket", got)
	}
}

func TestInHerdr(t *testing.T) {
	tests := []struct {
		name       string
		herdrEnv   string
		socketPath string
		want       bool
	}{
		{"managed pane", "1", "/run/herdr/herdr.sock", true},
		{"no HERDR_ENV", "", "/run/herdr/herdr.sock", false},
		{"HERDR_ENV not 1", "0", "/run/herdr/herdr.sock", false},
		// An empty socket path still resolves to the default under $HOME, so
		// it stays "in herdr" — the dial is what finally fails, and callers
		// degrade on that.
		{"default socket path", "1", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HERDR_ENV", tc.herdrEnv)
			t.Setenv("HERDR_SOCKET_PATH", tc.socketPath)
			if got := InHerdr(); got != tc.want {
				t.Errorf("InHerdr() = %v, want %v", got, tc.want)
			}
		})
	}
}
