//go:build unix

package main

import (
	"os/exec"
	"syscall"
)

// ownProcessGroup makes command the leader of its own process group and arranges
// for a cancelled context to kill that whole group, so the compiler children
// `go install` spawns die with it instead of outliving the timeout and holding
// the temporary module cache and binary directory open while they are removed.
func ownProcessGroup(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
}
