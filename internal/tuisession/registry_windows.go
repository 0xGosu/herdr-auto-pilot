//go:build windows

package tuisession

import (
	"errors"
	"os"
)

// Windows support is a follow-up (NFR-008), matching daemonlock: these stubs
// keep the build portable. Register fails fast on errUnsupported, so the TUI
// runs with no instance limit rather than not running at all.

var errUnsupported = errors.New("the TUI instance limit is not yet supported on Windows")

func claim(_ string) (*os.File, error) { return nil, errUnsupported }

func release(_ *os.File) {}

func held(_ string) (bool, error) { return false, errUnsupported }

func signalStop(_ int) error { return errUnsupported }

// processIdentity is unknown here, which disables the cross-namespace
// comparison — harmless, since no session can register in the first place.
func processIdentity() string { return "" }
