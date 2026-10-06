//go:build windows

package main

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// ownProcessGroup is the Windows counterpart. Windows has neither Setpgid nor
// kill(2) on a negative pid: CREATE_NEW_PROCESS_GROUP is the nearest equivalent
// to leading a group, and a cancelled context kills the installer process itself.
//
// Compiler children are not reaped along with it — that would need a job object
// — so a timeout here can briefly leave a compiler running. That is acceptable
// because this check is a Linux CI gate; the Windows build exists so that
// `go build ./...` stays green on the platform, which the cross-compile gate in
// tests.yml requires of every package in the module.
func ownProcessGroup(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
	command.Cancel = func() error { return command.Process.Kill() }
}
