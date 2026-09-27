//go:build windows

package main

import (
	"errors"
	"syscall"
	"testing"
)

// requireMountedInterruptSupport skips TestHarvestInterruptMounted on
// windows. The test's entire subject is POSIX process interruption: its
// fake git is a #!/bin/sh script (windows cannot execute a shebang script
// directly), the script is held open via a FIFO (windows has no FIFO), and
// the harvest child is stopped by delivering a real signal to its POSIX
// process group (windows processes have no process groups and support
// termination, not signal delivery). None of that has a windows equivalent,
// so there is no coverage to preserve here. See the unix build of this
// helper for what runs there.
func requireMountedInterruptSupport(t *testing.T) {
	t.Helper()
	t.Skip("TestHarvestInterruptMounted requires POSIX FIFOs, process groups, and signal delivery to interrupt a #!/bin/sh fake git script; windows has none of these")
}

// The helpers below are unreachable in practice: requireMountedInterruptSupport
// always skips above before any of them would run. They exist only so this
// windows test binary compiles.

func mkfifoForTest(t *testing.T, path string) {
	t.Helper()
	t.Fatal("mkfifoForTest: no FIFO on windows; requireMountedInterruptSupport should have skipped before this point")
}

func interruptGroupAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{}
}

func killInterruptGroup(pid int) {}

func openFIFOWriterNonblock(path string) (fd int, opened bool, err error) {
	return 0, false, errors.New("openFIFOWriterNonblock: no FIFO on windows")
}
