//go:build windows

package control

import (
	"context"
	"errors"
	"net"
)

// The control channel is abstracted so the daemon/action path has no
// platform-locked API (NFR-008): Windows will use a named pipe here. The
// MVP targets Linux and macOS; this stub keeps the Windows build compiling.

var errWindowsPending = errors.New("control channel: Windows named-pipe transport not yet implemented (MVP targets Linux/macOS)")

func listen(path string) (net.Listener, error) { return nil, errWindowsPending }

func dial(ctx context.Context, path string) (net.Conn, error) { return nil, errWindowsPending }
