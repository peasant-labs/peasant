//go:build unix

package storetest

import (
	"golang.org/x/sys/unix"
)

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

// cacheSpaceOK reports whether the template cache directory's filesystem has
// room for one more standing template. Below the floor (or when the query
// itself fails) the helper skips caching and builds privately, so a
// nearly-full filesystem never gains a new standing file from tests.
func cacheSpaceOK(dir string) bool {
	free, ok := freeBytesOnFilesystem(dir)
	return ok && free >= minFreeBytesForCache
}
