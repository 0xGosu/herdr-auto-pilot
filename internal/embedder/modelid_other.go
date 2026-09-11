//go:build !linux && !darwin

package embedder

import "os"

// fileIdentity has no inode or change time to offer on this platform (hap
// ships for linux and macOS); size and mtime still guard the entry.
func fileIdentity(os.FileInfo) (inode, device uint64, ctimeNs int64) { return 0, 0, 0 }
