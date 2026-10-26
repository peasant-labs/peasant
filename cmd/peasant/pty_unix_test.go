//go:build unix

package main

import (
	"syscall"
	"testing"
)

// ptyIoctl issues the raw ioctl(2) that openTestTerminal (see
// prune_exact_test.go) needs to unlock and number a Linux pseudo-terminal
// (TIOCSPTLCK / TIOCGPTN). It exists only so that call — syscall.SYS_IOCTL
// plus the three-argument syscall.Syscall it requires — lives on the
// platforms that actually have it. Windows has neither, so the raw call is
// kept here rather than in the shared file. See the windows build of this
// helper.
func ptyIoctl(t *testing.T, fd, request, arg uintptr) (r1, r2 uintptr, errno syscall.Errno) {
	t.Helper()
	return syscall.Syscall(syscall.SYS_IOCTL, fd, request, arg)
}
