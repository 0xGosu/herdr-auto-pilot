//go:build integration

package integration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/herdr"
)

// requireHerdrSocket skips unless herdr injected its control socket, i.e. we
// are running inside a herdr-managed pane.
func requireHerdrSocket(t *testing.T) {
	t.Helper()
	requireHerdr(t)
	if os.Getenv("HERDR_SOCKET_PATH") == "" {
		t.Skip("HERDR_SOCKET_PATH not set; run this from inside a herdr-managed pane")
	}
}

// TestRealHerdrNotification drives the SocketNotifier against a LIVE herdr.
// The unit suite fakes the socket, so only this catches drift in the
// notification.show contract the TUI's bell fallback is built on: the result
// must carry a `reason` the TUI knows, and `shown` must agree with it.
//
// It raises a real toast on the operator's screen — that is the point.
func TestRealHerdrNotification(t *testing.T) {
	requireHerdrSocket(t)
	n := herdr.NewSocketNotifier(herdr.SocketPath())
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	res, err := n.ShowNotification(ctx,
		"hap integration test", "TestRealHerdrNotification — safe to dismiss")
	if err != nil {
		t.Fatalf("ShowNotification against real herdr: %v", err)
	}
	if res.Reason == "" {
		t.Error("herdr returned no reason — the result shape changed")
	}
	// shown can legitimately be false (toasts disabled, rate limited, no
	// foreground client, a toast already standing) — that is the fallback
	// path, not a failure. Only an unrecognized reason means real drift.
	switch res.Reason {
	case "shown", "disabled", "rate_limited", "no_foreground_client", "busy":
	default:
		t.Errorf("unrecognized reason %q — herdr added a case the TUI does not know", res.Reason)
	}
	if res.Shown != (res.Reason == "shown") {
		t.Errorf("shown=%v disagrees with reason=%q", res.Shown, res.Reason)
	}
	t.Logf("notification.show -> shown=%v reason=%q", res.Shown, res.Reason)
}

// TestRealHerdrNotificationSecondCallSucceeds pins the reason every call opens
// its own connection: herdr 0.7.3 CLOSES the connection after answering a
// notification.show, so a pooled or reused connection would fail with EPIPE on
// the second toast — and the TUI can raise two from one refresh (a new
// escalation and an externally-caused pause).
func TestRealHerdrNotificationSecondCallSucceeds(t *testing.T) {
	requireHerdrSocket(t)
	n := herdr.NewSocketNotifier(herdr.SocketPath())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	for i, title := range []string{"hap integration test 1/2", "hap integration test 2/2"} {
		if _, err := n.ShowNotification(ctx, title, "safe to dismiss"); err != nil {
			t.Fatalf("call %d failed — connections are not being reopened per call: %v", i+1, err)
		}
	}
}

// TestRealHerdrNotificationRejectsEmptyTitle: real herdr answers
// invalid_params for a title with no visible text. The adapter refuses such a
// title locally, before dialling — this asserts BOTH halves still agree, so
// the local guard can never drift away from the contract it stands in for.
func TestRealHerdrNotificationRejectsEmptyTitle(t *testing.T) {
	requireHerdrSocket(t)
	n := herdr.NewSocketNotifier(herdr.SocketPath())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := n.ShowNotification(ctx, "   ", "body"); err == nil {
		t.Fatal("a whitespace-only title must be rejected locally")
	}

	// Now prove herdr itself still enforces the rule, with a title our local
	// clamp CANNOT catch: control characters are not unicode space, so
	// strings.Fields keeps them, but herdr normalizes them away and is left
	// with nothing. A successful call here means the contract relaxed.
	_, err := n.Show(ctx, herdr.Notification{Title: "\x01\x02"})
	if err == nil {
		t.Skip("herdr accepted a control-character-only title; the contract relaxed")
	}
	var se *herdr.SocketError
	if !errors.As(err, &se) {
		t.Fatalf("want a *herdr.SocketError from herdr, got %T: %v", err, err)
	}
	if se.Code != "invalid_params" {
		t.Errorf("herdr rejected an empty title with %q, want invalid_params", se.Code)
	}
}

// TestRealInHerdrDetection asserts the runtime detection the whole feature
// hangs off: inside a herdr-managed pane HERDR_ENV=1 and HERDR_SOCKET_PATH
// both exist, and the resolved socket is a real file.
func TestRealInHerdrDetection(t *testing.T) {
	if os.Getenv("HERDR_ENV") != "1" {
		t.Skip("not running inside a herdr-managed pane")
	}
	if !herdr.InHerdr() {
		t.Fatal("InHerdr() is false inside a herdr-managed pane")
	}
	path := herdr.SocketPath()
	if path == "" {
		t.Fatal("SocketPath() is empty inside herdr")
	}
	if !strings.HasSuffix(path, ".sock") {
		t.Errorf("socket path %q does not look like a socket", path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("resolved socket %q does not exist: %v", path, err)
	}
	if got := filepath.Clean(path); got != path {
		t.Errorf("socket path %q is not clean", path)
	}
}
