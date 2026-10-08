//go:build unix

package store

import (
	"golang.org/x/sys/unix"
)

// migrateFreeBytes reports the bytes available to the caller on the
// filesystem holding path. ok is false when the query itself fails, in
// which case the preflight warns instead of refusing: correctness never
// depends on the guard.
func migrateFreeBytes(path string) (free uint64, ok bool) {
	if path == "" {
		return 0, false
	}
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, false
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), true
}
