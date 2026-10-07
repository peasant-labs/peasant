//go:build unix

package store

import (
	"io/fs"
	"syscall"
)

// fileIdentity returns the inode number identifying info's underlying file and
// whether this platform exposes inode identity at all. Windows has none, so its
// build of this helper reports false and callers skip the assertion.
//
// The type assertion to *syscall.Stat_t has to live behind a build tag rather
// than in a shared file: syscall.Stat_t does not exist on Windows, so a shared
// file referencing it fails to COMPILE there, and a runtime fallback guarding it
// never gets the chance to run.
func fileIdentity(info fs.FileInfo) (uint64, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(stat.Ino), true
}
