package herdr

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// fallbackNotifyBudget bounds the whole socket-then-CLI chain. It matches the
// CLI adapter's own command timeout so adding the socket hop can never make
// the worst case slower than the CLI path was before this chain existed.
const fallbackNotifyBudget = 15 * time.Second

// FallbackNotifier prefers herdr's socket `notification.show` — the only path
// that reports whether the toast was actually painted (IR-003) — and drops to
// the CLI `notification show` when the socket round trip fails outright.
//
// The fallback covers TRANSPORT failure only: no socket path, herdr restarted
// under us, a dial that timed out. A toast herdr ANSWERED and declined
// (disabled, rate_limited, no_foreground_client, busy) is never retried
// through the CLI — that reaches the same herdr and is declined the same way,
// and re-firing would turn one dropped toast into two dropped toasts while
// throwing away the very fact the socket path exists to report.
//
// After a CLI fallback the outcome is unknowable, so the result is left
// !Known rather than claimed as delivered.
type FallbackNotifier struct {
	// Socket is the preferred, delivery-reporting path. Nil disables it.
	Socket ports.NotifyShower
	// CLI is the transport-failure backstop. Nil disables it.
	CLI ports.NotifyPort
}

// Compile-time conformance: the chain is usable anywhere either port is.
var (
	_ ports.NotifyPort   = (*FallbackNotifier)(nil)
	_ ports.NotifyShower = (*FallbackNotifier)(nil)
)

// NewFallbackNotifier chains a socket notifier for the given path ahead of an
// existing CLI notifier. An empty socketPath yields a chain that is CLI-only,
// which is exactly the degraded behavior wanted when herdr never told us where
// its socket lives.
func NewFallbackNotifier(socketPath string, cli ports.NotifyPort) *FallbackNotifier {
	f := &FallbackNotifier{CLI: cli}
	if socketPath != "" {
		f.Socket = NewSocketNotifier(socketPath)
	}
	return f
}

// ShowNotification satisfies ports.NotifyShower.
func (f *FallbackNotifier) ShowNotification(ctx context.Context, title, body string) (ports.NotifyResult, error) {
	// One budget for the WHOLE chain, not one per hop. Without this a herdr
	// that accepts the connection and never answers costs the socket's 3s and
	// THEN the CLI adapter's 15s, and escalate runs on the daemon's main
	// select loop (CLAUDE.md: don't stall the main loop) — so the fallback
	// would make the worst case worse than the CLI alone ever was.
	ctx, cancel := context.WithTimeout(ctx, fallbackNotifyBudget)
	defer cancel()

	var socketErr error
	if f.Socket != nil {
		res, err := f.Socket.ShowNotification(ctx, title, body)
		if err == nil {
			return res, nil
		}
		if f.CLI == nil || !worthRetryingOnCLI(ctx, err) {
			return ports.NotifyResult{}, err
		}
		socketErr = err
		// Warn, not Debug: a permanently wrong socket path silently costs the
		// operator every delivery report from here on, and the CLI's exit-0
		// success would otherwise hide that completely.
		slog.Warn("herdr notification socket failed, falling back to CLI",
			"error", err, "title", title)
	}
	if f.CLI == nil {
		return ports.NotifyResult{}, fmt.Errorf("no notifier configured")
	}
	if err := f.CLI.Notify(ctx, title, body); err != nil {
		// Join so the Warn the caller logs names the transport cause too;
		// without it a failed chain reports only the subprocess failure and
		// the reason the socket was abandoned is lost.
		return ports.NotifyResult{}, errors.Join(socketErr, err)
	}
	// Delivered to herdr, but `notification show` exits 0 either way: we have
	// no evidence a toast was painted. The ZERO result is the unknown one, so
	// returning it keeps `res == ports.NotifyResult{}` meaning "no idea" and
	// never puts a Reason herdr did not give in front of a caller.
	return ports.NotifyResult{}, nil
}

// worthRetryingOnCLI decides whether a failed socket attempt deserves a second
// channel. Only a request that never got an answer does.
//
// A herdr-ANSWERED refusal (*SocketError) means herdr was reached and said no;
// re-firing the identical request through `herdr notification show` reaches the
// same herdr, is refused the same way, and costs a subprocess plus its timeout
// while burying the real code behind a Warn. The one exception is a method the
// server does not know: an older herdr without `notification.show` is exactly
// the capability gap the CLI backstop exists for.
//
// A cancelled caller ctx (daemon shutting down) is not a herdr fault either —
// the CLI would inherit the same dead ctx and fail too, so it buys two
// failures and an extra Warn on the way out.
func worthRetryingOnCLI(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	// A local refusal never reached herdr, but it is a caller bug: the CLI
	// would send the same invalid request and herdr would reject it again.
	if errors.Is(err, ErrEmptyNotificationTitle) {
		return false
	}
	var se *SocketError
	if errors.As(err, &se) {
		return se.Code == "method_not_found"
	}
	return true
}

// Notify satisfies ports.NotifyPort. As with SocketNotifier, a toast herdr
// declined to paint is not an error — only a failure to complete the request
// is.
func (f *FallbackNotifier) Notify(ctx context.Context, title, body string) error {
	_, err := f.ShowNotification(ctx, title, body)
	return err
}
