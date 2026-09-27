//go:build !unix

package testgate

import "time"

// readChildUsage is unavailable on this platform (getrusage is Unix-only), so
// per-unit CPU is reported as zero and only wall is measured.
func readChildUsage() (time.Duration, time.Duration, bool) {
	return 0, 0, false
}
