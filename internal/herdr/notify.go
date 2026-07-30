package herdr

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// Sound values `notification.show` accepts. Anything else is rejected by
// herdr, so callers pick from these.
const (
	SoundNone    = "none"
	SoundDone    = "done"
	SoundRequest = "request"
)

// Herdr's own text normalization trims a notification's title to 80 display
// characters and its body to 240. We clamp locally to the same bounds so what
// we log and what herdr paints are the same string, and so a huge pane
// excerpt is never written to the socket in the first place.
const (
	maxNotifyTitle = 80
	maxNotifyBody  = 240
)

// defaultNotifyTimeout bounds one notification round trip. A toast is a
// courtesy: it must never hold up the caller (the TUI dispatches these from
// its update loop's goroutine pool), so this is far shorter than the CLI
// adapter's 15s command timeout.
const defaultNotifyTimeout = 3 * time.Second

// ErrEmptyNotificationTitle is the local refusal of a title herdr would reject
// with invalid_params. It is a caller bug, not a transport fault, so a chain
// must recognize it and decline to spend a second channel on the same
// invalid request (see FallbackNotifier).
var ErrEmptyNotificationTitle = errors.New("notification title is empty")

// SocketPath resolves herdr's control socket the way herdr itself documents:
// HERDR_SOCKET_PATH when herdr injected it (every managed pane gets one),
// else the default session socket under the user's config dir.
func SocketPath() string {
	if p := os.Getenv("HERDR_SOCKET_PATH"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "herdr", "herdr.sock")
}

// InHerdr reports whether this process is running as a herdr-managed pane.
// Herdr injects HERDR_ENV=1 into every process it launches, which is the
// signal to use its socket API rather than plain-terminal fallbacks; a socket
// we cannot even name means there is nothing to talk to.
func InHerdr() bool {
	return os.Getenv("HERDR_ENV") == "1" && SocketPath() != ""
}

// Notification is one `notification.show` request. Only Title is required.
type Notification struct {
	Title string
	Body  string
	// Position is the desktop placement ("top-left", …); empty uses herdr's
	// configured default.
	Position string
	// Sound is one of SoundNone / SoundDone / SoundRequest; empty means
	// herdr's default (none).
	Sound string
}

// SocketNotifier raises operator notifications over herdr's control socket
// (`notification.show`). Unlike the CLI adapter's Notify it reports whether
// herdr actually PAINTED the toast, so a caller can fall back to another
// channel when herdr drops it (toasts disabled, rate limited, no foreground
// client, or an in-app toast already standing).
//
// Every call opens its own connection. That is not just simplicity: verified
// against herdr 0.7.3, the server CLOSES the connection right after answering
// a notification.show — on the error path and the success path alike — so a
// second request written to the same connection dies with EPIPE. Do not
// "optimize" this into a persistent connection.
type SocketNotifier struct {
	SocketPath string
	// Timeout bounds one round trip; zero means defaultNotifyTimeout.
	Timeout time.Duration
	// Dial allows tests to substitute the transport, mirroring Subscriber.
	Dial func(ctx context.Context) (net.Conn, error)

	seq atomic.Uint64
}

// Compile-time conformance: the socket notifier satisfies both the plain and
// the result-reporting notification ports.
var (
	_ ports.NotifyPort   = (*SocketNotifier)(nil)
	_ ports.NotifyShower = (*SocketNotifier)(nil)
)

// NewSocketNotifier creates a notifier for the given herdr socket path.
// Dial is deliberately left nil: the zero value already dials the real
// socket (see dial), so a literal-constructed notifier behaves identically.
func NewSocketNotifier(socketPath string) *SocketNotifier {
	return &SocketNotifier{SocketPath: socketPath, Timeout: defaultNotifyTimeout}
}

func (n *SocketNotifier) timeout() time.Duration {
	if n.Timeout <= 0 {
		return defaultNotifyTimeout
	}
	return n.Timeout
}

// Show sends one notification and reports what herdr did with it. A non-nil
// error means the request never completed; a nil error with Shown == false
// means herdr answered and declined to display it (see NotifyResult.Reason).
func (n *SocketNotifier) Show(ctx context.Context, msg Notification) (ports.NotifyResult, error) {
	title := clampNotifyText(msg.Title, maxNotifyTitle)
	if title == "" {
		// herdr rejects an empty normalized title with invalid_params; catch
		// it here so a caller's bug surfaces as its own error rather than a
		// protocol one, and so we never open a connection for nothing.
		return ports.NotifyResult{}, ErrEmptyNotificationTitle
	}
	params := map[string]any{"title": title}
	if body := clampNotifyText(msg.Body, maxNotifyBody); body != "" {
		params["body"] = body
	}
	if msg.Position != "" {
		params["position"] = msg.Position
	}
	if msg.Sound != "" {
		params["sound"] = msg.Sound
	}

	ctx, cancel := context.WithTimeout(ctx, n.timeout())
	defer cancel()

	var result struct {
		Shown  bool   `json:"shown"`
		Reason string `json:"reason"`
	}
	id := fmt.Sprintf("hap_notify_%d", n.seq.Add(1))
	if err := call(ctx, n.dial, id, "notification.show", params, &result); err != nil {
		return ports.NotifyResult{}, err
	}
	// Known: herdr answered this one, so Shown is evidence rather than a guess.
	return ports.NotifyResult{Shown: result.Shown, Reason: result.Reason, Known: true}, nil
}

// ShowNotification satisfies ports.NotifyShower — the title/body form the
// front-ends use, with the "request" sound that marks an operator action.
func (n *SocketNotifier) ShowNotification(ctx context.Context, title, body string) (ports.NotifyResult, error) {
	return n.Show(ctx, Notification{Title: title, Body: body, Sound: SoundRequest})
}

// Notify satisfies ports.NotifyPort. A toast herdr declined to display is NOT
// an error here — that distinction is exactly what ShowNotification exists
// for, and reporting it as a failure would make every rate-limited toast look
// like a broken socket.
func (n *SocketNotifier) Notify(ctx context.Context, title, body string) error {
	_, err := n.ShowNotification(ctx, title, body)
	return err
}

// dial routes through the overridable field, defaulting when it is nil so a
// zero-valued SocketNotifier (constructed as a literal, as tests do) still
// works.
func (n *SocketNotifier) dial(ctx context.Context) (net.Conn, error) {
	if n.Dial != nil {
		return n.Dial(ctx)
	}
	d := net.Dialer{Timeout: n.timeout()}
	return d.DialContext(ctx, "unix", n.SocketPath)
}

// clampNotifyText applies herdr's own normalization — collapse every run of
// whitespace (newlines and tabs included) to a single space, trim, then cut
// to max RUNES — so the caller sees the same text herdr will.
func clampNotifyText(s string, max int) string {
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 0 {
		return ""
	}
	if max == 1 {
		return "…"
	}
	return string(r[:max-1]) + "…"
}
