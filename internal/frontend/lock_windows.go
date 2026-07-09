//go:build windows

package frontend

// lockFile is a no-op on Windows for now (MVP targets Linux/macOS; the
// atomic temp+rename save still prevents file corruption there).
func lockFile(path string) (func(), error) {
	return func() {}, nil
}
