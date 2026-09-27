//go:build windows

package main

import (
	"syscall"
	"testing"
)

// ptyIoctl has no windows implementation: windows defines neither
// syscall.SYS_IOCTL nor the three-argument syscall.Syscall signature that
// Linux pseudo-terminal allocation (TIOCSPTLCK / TIOCGPTN) needs. In practice
// this is never reached on windows anyway, because every caller first obtains
// its terminal from openTestTerminal (see prune_exact_test.go), which already
// skips there since the /dev/ptmx pseudo-terminal it allocates is
// Linux-specific. This stub exists only so the windows test binary compiles;
// it fails loudly if it is ever actually reached, since that would mean the
// earlier skip regressed. See the unix build of this helper.
func ptyIoctl(t *testing.T, fd, request, arg uintptr) (r1, r2 uintptr, errno syscall.Errno) {
	t.Helper()
	t.Fatal("ptyIoctl: no ioctl equivalent on windows; openTestTerminal should have skipped before this point")
	return 0, 0, 0
}
