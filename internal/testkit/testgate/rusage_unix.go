//go:build unix

package testgate

import (
	"syscall"
	"time"
)

// readChildUsage returns the accumulated user and system CPU time of the
// process's reaped children, via getrusage(RUSAGE_CHILDREN).
//
// Summed as the delta across one invocation, this is how a test binary's own
// CPU and the go-build children it spawns are attributed to the unit. The
// counter is process-global: under concurrent invocations the delta captures
// any sibling reaped during the window, so per-unit user/system are best-effort
// when Concurrency > 1. Wall is always exact.
func readChildUsage() (time.Duration, time.Duration, bool) {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_CHILDREN, &ru); err != nil {
		return 0, 0, false
	}
	return timeval(ru.Utime), timeval(ru.Stime), true
}

func timeval(tv syscall.Timeval) time.Duration {
	return time.Duration(tv.Sec)*time.Second + time.Duration(tv.Usec)*time.Microsecond
}
