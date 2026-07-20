//go:build windows

package taskfile

import "time"

// Lock is a no-op on Windows for now (MVP targets Linux/macOS; the atomic
// temp+rename save still prevents file corruption there).
func Lock(path string) (func(), error) {
	return func() {}, nil
}

// LockWithin is Lock with a deadline; with no locking to wait for, it cannot
// time out.
func LockWithin(path string, wait time.Duration) (func(), error) {
	return Lock(path)
}
