//go:build !windows

package control

import (
	"context"
	"net"
	"os"
	"time"
)

// listen creates the Unix-domain control socket with owner-only access.
func listen(path string) (net.Listener, error) {
	// Remove a stale socket from a previous daemon run.
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// dial connects to the Unix-domain control socket.
func dial(ctx context.Context, path string) (net.Conn, error) {
	d := net.Dialer{Timeout: 2 * time.Second}
	return d.DialContext(ctx, "unix", path)
}
