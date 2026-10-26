//go:build windows

package store

// migrateFreeBytes cannot query free space on Windows (Statfs is
// unix-only), so it reports unknown: the preflight warns instead of
// refusing, and correctness never depends on the guard.
func platformMigrateFreeBytes(_ string) (uint64, bool) {
	return 0, false
}
