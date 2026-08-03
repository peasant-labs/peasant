//go:build e2e && linux

package e2e

import (
	"os/exec"
	"runtime"
	"syscall"
)

// lockVillageLaunchThread intentionally has no matching runtime.UnlockOSThread.
// The village launcher goroutine must stay bonded to this OS thread for the
// village lifetime: unlocking lets the Go runtime retire the M, which can fire
// Pdeathsig early and reintroduce the premature-death flake. See golang/go#27505.
func lockVillageLaunchThread() {
	runtime.LockOSThread()
}

func setVillageProcDeath(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}
