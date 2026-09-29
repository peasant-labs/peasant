//go:build !unix

package storetest

import (
	"strconv"
	"strings"
)

// processAlive is conservative on platforms where the helper cannot probe a
// PID without signalling it: an unknown process is assumed alive, so the
// dead-owner sweep reaps only by the age rule there. That is still correct —
// a live owner's directory is never removed — and a reboot clears tmpfs
// litter on every platform that offers it.
func processAlive(_ int) bool { return true }

// parseOwnerPID extracts the pid from a managed-root entry named pid-<pid>.
// It reports false for any other name so the sweep never touches entries the
// helper did not create.
func parseOwnerPID(name string) (int, bool) {
	pid, ok := strings.CutPrefix(name, "pid-")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(pid)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}
