//go:build !unix

package storetest

// cacheSpaceOK cannot query free space on this platform (Statfs is
// unix-only), so it reports room: the cache is per checkout and CI checkouts
// are writable, and correctness never depends on the guard — the only loss
// without it is the skip-caching protection on a nearly-full disk.
func cacheSpaceOK(_ string) bool { return true }
