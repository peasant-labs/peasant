//go:build !unix

package storetest

// minFreeBytesForCache is the free-space floor for both the template cache
// and the tmpfs copy root. This platform cannot query filesystem free space
// or detect tmpfs (Statfs is unix-only), so the helpers below report
// "unknown": the cache falls back to private temp builds and the copy root
// falls back to t.TempDir, which is correct everywhere — the only loss is
// RAM-copy speed, restorable with PEASANT_STORETEST_TMPDIR.
const minFreeBytesForCache = 256 << 20

func freeBytesOnFilesystem(_ string) (uint64, bool) { return 0, false }

func isTmpfsPath(_ string) bool { return false }

// cacheSpaceOK cannot query free space on this platform (Statfs is
// unix-only), so it reports room: the cache is per checkout and CI checkouts
// are writable, and correctness never depends on the guard — the only loss
// without it is the skip-caching protection on a nearly-full disk.
func cacheSpaceOK(_ string) bool { return true }
