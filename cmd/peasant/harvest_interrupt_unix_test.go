//go:build unix

package main

import (
	"errors"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// requireMountedInterruptSupport is a no-op on unix: TestHarvestInterruptMounted's
// mechanism is native here. See the windows build of this helper for what it
// depends on and why that mechanism has no windows equivalent.
func requireMountedInterruptSupport(t *testing.T) {}

// mkfifoForTest creates the POSIX FIFO TestHarvestInterruptMounted uses to
// hold its fake git script open until the test is ready to release it.
func mkfifoForTest(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
}

// interruptGroupAttr puts the harvest child in its own process group so the
// whole tree it spawns can be killed together by the negated pid.
func interruptGroupAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// killInterruptGroup force-kills the harvest child's whole process group.
func killInterruptGroup(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

// openFIFOWriterNonblock opens path for a nonblocking write, reporting "not
// ready yet" (no reader present on the FIFO) rather than treating that as an
// error.
func openFIFOWriterNonblock(path string) (fd int, opened bool, err error) {
	n, openErr := unix.Open(path, unix.O_WRONLY|unix.O_NONBLOCK, 0)
	if openErr == nil {
		return n, true, nil
	}
	if errors.Is(openErr, unix.ENXIO) {
		return 0, false, nil
	}
	return 0, false, openErr
}
