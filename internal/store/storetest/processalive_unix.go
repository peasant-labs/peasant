//go:build unix

package storetest

import (
	"errors"
	"strconv"
	"strings"
	"syscall"
)

// processAlive reports whether pid names a process that may still be running.
// Kill(pid, 0) performs no signalling: a nil error means the process exists,
// EPERM means it exists but belongs to another user, and ESRCH means it is
// definitely dead. Any other outcome is treated as alive — the dead-owner
// sweep must never reap a live owner's directory.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

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
