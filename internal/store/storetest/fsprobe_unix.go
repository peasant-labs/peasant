//go:build unix

package storetest

import (
	"golang.org/x/sys/unix"
)

// minFreeBytesForCache is the free-space floor for both the template cache
// and the tmpfs copy root: below it the helper falls back to private temp
// builds and t.TempDir copies so a nearly-full filesystem never gains a new
// standing file from tests.
const minFreeBytesForCache = 256 << 20

// freeBytesOnFilesystem reports the bytes available to the caller on the
// filesystem holding path. ok is false when the query itself fails, in which
// case the caller falls back.
func freeBytesOnFilesystem(path string) (free uint64, ok bool) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, false
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), true
}

// isTmpfsPath reports whether path lives on a tmpfs mount: the RAM-backed
// filesystem the managed copy root prefers.
func isTmpfsPath(path string) bool {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return false
	}
	// _TMPFS_MAGIC (0x01021994) from <linux/magic.h>.
	return stat.Type == 0x01021994
}

// cacheSpaceOK reports whether the cache directory's filesystem has room for
// one more standing template. Below the floor (or when the query itself
// fails) the helper skips caching and builds privately, so a nearly-full
// filesystem never gains a new standing file from tests.
func cacheSpaceOK(dir string) bool {
	free, ok := freeBytesOnFilesystem(dir)
	return ok && free >= minFreeBytesForCache
}
