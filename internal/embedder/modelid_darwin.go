//go:build darwin

package embedder

import (
	"os"
	"syscall"
)

// fileIdentity is the file's inode, device and change time; see the linux
// twin for why the change time is the fact that matters.
func fileIdentity(fi os.FileInfo) (inode, device uint64, ctimeNs int64) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Ino, uint64(st.Dev), st.Ctimespec.Nano()
	}
	return 0, 0, 0
}
