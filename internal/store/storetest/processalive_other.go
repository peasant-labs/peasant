//go:build !unix

package storetest

import "time"

// processAlive is conservative on platforms where the helper cannot probe a
// PID without signalling it: an unknown process is assumed alive, so the
// dead-owner sweep reaps only by the age rule there. That is still correct —
// a live owner's directory is never removed — and a reboot clears tmpfs
// litter on every platform that offers it.
func processAlive(_ int) bool { return true }

// reapableOwner is age-only where liveness cannot be probed: with no
// dead/alive signal the bias is leak-not-reap, so only entries older than a
// full day qualify. No test suite spans that, while killed-run litter is
// still eventually reclaimed.
func reapableOwner(_ int, modTime, now time.Time) bool {
	return now.Sub(modTime) >= ownerUnknownMaxAge
}
