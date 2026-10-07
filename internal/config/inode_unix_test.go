//go:build unix

package config_test

import (
	"io/fs"
	"syscall"
)

// fileIdentity returns the inode number identifying info's underlying file and
// whether this platform exposes inode identity at all. Windows has no inode, so
// its build of this helper reports false and callers skip the assertion.
func fileIdentity(info fs.FileInfo) (uint64, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(stat.Ino), true
}
