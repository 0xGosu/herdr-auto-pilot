//go:build linux

package embedder

import (
	"os"
	"syscall"
)

// fileIdentity is the file's inode, device and change time. The change time
// is the one fact user space cannot set: a model rewritten in place at the same
// size with its mtime put back (`cp -p`, `touch -r`, an archive tool storing
// whole seconds) keeps size, mtime and inode, and only ctime moves.
func fileIdentity(fi os.FileInfo) (inode, device uint64, ctimeNs int64) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Ino, st.Dev, st.Ctim.Nano()
	}
	return 0, 0, 0
}
