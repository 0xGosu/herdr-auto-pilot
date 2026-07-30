package herdr

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// recordingCLI stands in for the CLI adapter's Notify — the transport
// backstop. Guarded because the chain is exercised concurrently under -race.
type recordingCLI struct {
	mu    sync.Mutex
	calls [][2]string
	err   error
}

func (r *recordingCLI) Notify(_ context.Context, title, body string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, [2]string{title, body})
	return r.err
}

func (r *recordingCLI) seen() [][2]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][2]string(nil), r.calls...)
}

func TestFallbackNotifierPrefersSocketAndReportsDelivery(t *testing.T) {
	n, srv := newTestNotifier(t)
	cli := &recordingCLI{}
	f := &FallbackNotifier{Socket: n, CLI: cli}

	res, err := f.ShowNotification(context.Background(), "build failed", "api workspace")
	if err != nil {
		t.Fatalf("ShowNotification: %v", err)
	}
	if !res.Shown || !res.Known {
		t.Fatalf("want a reported, displayed toast, got %+v", res)
	}
	if len(srv.SocketNotifications()) != 1 {
		t.Errorf("socket should have carried the toast, got %d", len(srv.SocketNotifications()))
	}
	if len(cli.seen()) != 0 {
		t.Errorf("CLI must not run when the socket succeeded, got %v", cli.seen())
	}
}

// TestFallbackNotifierDeclinedToastIsNotRetriedOnCLI is the invariant that
// keeps this a FALLBACK and not a retry loop. herdr answering "I dropped it"
// is a completed round trip; re-firing through the CLI reaches the same herdr,
// is declined the same way, and throws away the delivery report that is the
// entire reason for preferring the socket.
func TestFallbackNotifierDeclinedToastIsNotRetriedOnCLI(t *testing.T) {
	n, srv := newTestNotifier(t)
	srv.SetNotificationResult(false, "no_foreground_client")
	cli := &recordingCLI{}
	f := &FallbackNotifier{Socket: n, CLI: cli}

	res, err := f.ShowNotification(context.Background(), "escalation", "agent blocked")
	if err != nil {
		t.Fatalf("a declined toast must not be an error, got %v", err)
	}
	if res.Shown {
		t.Error("Shown should be false")
	}
	if !res.Known {
		t.Error("herdr answered, so the outcome is Known")
	}
	if res.Reason != "no_foreground_client" {
		t.Errorf("Reason = %q, want the reason herdr gave", res.Reason)
	}
	if len(cli.seen()) != 0 {
		t.Fatalf("a declined toast must not fall back to the CLI, got %v", cli.seen())
	}
}

// TestFallbackNotifierTransportFailureFallsBackUnreported covers the case the
// fallback exists for: the socket never completed, so the CLI carries the
// toast — and because `notification show` exits 0 either way, the result must
// NOT claim delivery.
func TestFallbackNotifierTransportFailureFallsBackUnreported(t *testing.T) {
	dead := NewSocketNotifier(filepath.Join(t.TempDir(), "nonexistent.sock"))
	cli := &recordingCLI{}
	f := &FallbackNotifier{Socket: dead, CLI: cli}

	res, err := f.ShowNotification(context.Background(), "escalation", "agent blocked")
	if err != nil {
		t.Fatalf("the CLI carried it, so this is not an error: %v", err)
	}
	if res.Known {
		t.Errorf("the CLI cannot report delivery; Known must stay false, got %+v", res)
	}
	if res.Shown {
		t.Error("a result we have no evidence for must never claim Shown")
	}
	if len(cli.seen()) != 1 || cli.seen()[0][0] != "escalation" {
		t.Fatalf("CLI should have carried the toast, got %v", cli.seen())
	}
}

func TestFallbackNotifierTransportFailureWithNoCLIIsAnError(t *testing.T) {
	dead := NewSocketNotifier(filepath.Join(t.TempDir(), "nonexistent.sock"))
	f := &FallbackNotifier{Socket: dead}

	if _, err := f.ShowNotification(context.Background(), "x", "y"); err == nil {
		t.Fatal("want an error when neither path can carry the toast")
	}
}

func TestFallbackNotifierCLIErrorSurfaces(t *testing.T) {
	dead := NewSocketNotifier(filepath.Join(t.TempDir(), "nonexistent.sock"))
	want := errors.New("herdr notification show: exit 1")
	f := &FallbackNotifier{Socket: dead, CLI: &recordingCLI{err: want}}

	_, err := f.ShowNotification(context.Background(), "x", "y")
	if !errors.Is(err, want) {
		t.Fatalf("want the CLI's error, got %v", err)
	}
}

// TestNewFallbackNotifierEmptySocketPathIsCLIOnly: herdr never told us where
// its socket is, which must degrade to the historical CLI behavior rather than
// failing every toast on a dial to "".
func TestNewFallbackNotifierEmptySocketPathIsCLIOnly(t *testing.T) {
	cli := &recordingCLI{}
	f := NewFallbackNotifier("", cli)

	if f.Socket != nil {
		t.Fatal("no socket path must mean no socket notifier")
	}
	res, err := f.ShowNotification(context.Background(), "escalation", "agent blocked")
	if err != nil {
		t.Fatalf("ShowNotification: %v", err)
	}
	if res.Known {
		t.Error("CLI-only can never report delivery")
	}
	if len(cli.seen()) != 1 {
		t.Fatalf("CLI should have carried the toast, got %v", cli.seen())
	}
}

// TestFallbackNotifierHerdrAnsweredErrorIsNotRetried: a *SocketError means
// herdr was REACHED and refused. The CLI reaches the same herdr and is refused
// the same way, so spending a subprocess (and burying the real code behind a
// Warn) buys nothing. Only an unknown method — an older herdr genuinely
// lacking `notification.show` — is the capability gap the backstop is for.
func TestFallbackNotifierHerdrAnsweredErrorIsNotRetried(t *testing.T) {
	tests := []struct {
		name    string
		code    string
		wantCLI int
		wantErr bool
	}{
		{name: "invalid params is herdr's verdict", code: "invalid_params", wantCLI: 0, wantErr: true},
		{name: "unknown method falls back", code: "method_not_found", wantCLI: 1, wantErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n, srv := newTestNotifier(t)
			srv.SetNotificationError(tt.code, "nope")
			cli := &recordingCLI{}
			f := &FallbackNotifier{Socket: n, CLI: cli}

			_, err := f.ShowNotification(context.Background(), "escalation", "agent blocked")
			if tt.wantErr && err == nil {
				t.Fatal("want herdr's own error surfaced, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("want the CLI to have carried it, got %v", err)
			}
			if len(cli.seen()) != tt.wantCLI {
				t.Fatalf("CLI calls = %d, want %d", len(cli.seen()), tt.wantCLI)
			}
		})
	}
}

// A cancelled caller (daemon shutting down) must not spend a subprocess on a
// ctx the CLI would inherit and fail on too.
func TestFallbackNotifierCancelledContextDoesNotFallBack(t *testing.T) {
	n, _ := newTestNotifier(t)
	cli := &recordingCLI{}
	f := &FallbackNotifier{Socket: n, CLI: cli}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := f.ShowNotification(ctx, "escalation", "agent blocked"); err == nil {
		t.Fatal("want the cancellation surfaced")
	}
	if len(cli.seen()) != 0 {
		t.Fatalf("a cancelled ctx must not reach the CLI, got %v", cli.seen())
	}
}

// An empty title is a caller bug caught locally; the CLI would send the same
// invalid request for herdr to reject again.
func TestFallbackNotifierEmptyTitleDoesNotFallBack(t *testing.T) {
	n, _ := newTestNotifier(t)
	cli := &recordingCLI{}
	f := &FallbackNotifier{Socket: n, CLI: cli}

	_, err := f.ShowNotification(context.Background(), "   ", "body")
	if !errors.Is(err, ErrEmptyNotificationTitle) {
		t.Fatalf("want ErrEmptyNotificationTitle, got %v", err)
	}
	if len(cli.seen()) != 0 {
		t.Fatalf("an invalid request must not reach the CLI, got %v", cli.seen())
	}
}

// When BOTH paths fail the caller must still learn why the socket was
// abandoned — otherwise the daemon's Warn names only the subprocess failure.
func TestFallbackNotifierJoinsBothErrors(t *testing.T) {
	dead := NewSocketNotifier(filepath.Join(t.TempDir(), "nonexistent.sock"))
	cliErr := errors.New("herdr notification show: exit 1")
	f := &FallbackNotifier{Socket: dead, CLI: &recordingCLI{err: cliErr}}

	_, err := f.ShowNotification(context.Background(), "x", "y")
	if !errors.Is(err, cliErr) {
		t.Fatalf("want the CLI error preserved, got %v", err)
	}
	if !strings.Contains(err.Error(), "dial") && !strings.Contains(err.Error(), "socket") {
		t.Errorf("want the transport cause joined in too, got %q", err)
	}
}

func TestFallbackNotifierWithNoPathsConfigured(t *testing.T) {
	f := &FallbackNotifier{}
	if _, err := f.ShowNotification(context.Background(), "x", "y"); err == nil {
		t.Fatal("want an error when nothing can carry the toast")
	}
}

func TestNewFallbackNotifierBuildsASocketPath(t *testing.T) {
	_, srv := newTestNotifier(t)
	f := NewFallbackNotifier(srv.SocketPath, &recordingCLI{})
	if f.Socket == nil {
		t.Fatal("a non-empty socket path must build a socket notifier")
	}
	res, err := f.ShowNotification(context.Background(), "build failed", "api")
	if err != nil {
		t.Fatalf("ShowNotification: %v", err)
	}
	if !res.Known || !res.Shown {
		t.Fatalf("want the socket's reported delivery, got %+v", res)
	}
}

// Concurrency is an explicit design claim (no locking, fresh connection per
// call, atomic request ids). Run under -race to mean anything.
func TestFallbackNotifierConcurrentCalls(t *testing.T) {
	n, srv := newTestNotifier(t)
	f := &FallbackNotifier{Socket: n, CLI: &recordingCLI{}}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := f.ShowNotification(context.Background(), "escalation", "blocked"); err != nil {
				t.Errorf("concurrent ShowNotification: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := len(srv.SocketNotifications()); got != 10 {
		t.Fatalf("want all 10 toasts delivered, got %d", got)
	}
}

// TestFallbackNotifierNotifyIgnoresNotShown mirrors SocketNotifier.Notify: a
// toast herdr declined is not a broken notifier.
func TestFallbackNotifierNotifyIgnoresNotShown(t *testing.T) {
	n, srv := newTestNotifier(t)
	srv.SetNotificationResult(false, "disabled")
	f := &FallbackNotifier{Socket: n, CLI: &recordingCLI{}}

	if err := f.Notify(context.Background(), "escalation", "agent blocked"); err != nil {
		t.Fatalf("a declined toast must not be an error, got %v", err)
	}
}

// Conformance: the chain must be usable wherever either port is expected.
var (
	_ ports.NotifyPort   = (*FallbackNotifier)(nil)
	_ ports.NotifyShower = (*FallbackNotifier)(nil)
)
